package kiro

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// StreamEvent 流式事件类型
type StreamEvent struct {
	Type      string // "text", "tool_start", "tool_input", "tool_stop"
	Text      string // type=text 时的文本内容
	ToolUseID string // tool 相关事件的 ID
	ToolName  string // type=tool_start 时的工具名
	Input     string // type=tool_input 时的 JSON 片段
}

// StreamCallback is called for each streaming event received from Kiro
type StreamCallback func(event StreamEvent)

// SendRequestStream sends a request to Kiro and streams text chunks via callback.
// Returns the final KiroResponse (with full content, credits, etc.) after the stream ends.
func (c *KiroClient) SendRequestStream(
	ctx context.Context,
	token, machineID, model, userContent string,
	history, tools, images, toolResults []interface{},
	profileArn string,
	onChunk StreamCallback,
	proxyTransport ...http.RoundTripper,
) (*KiroResponse, int, error) {
	headers := c.BuildHeaders(token, "vibe", machineID)
	body := c.BuildRequest(userContent, model, history, tools, images, toolResults, profileArn)

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal request: %w", err)
	}
	bodyBytes = CleanNullBytes(bodyBytes)

	// 保存每次发给 Kiro 的请求体到日志文件
	SaveBodyToFile("kiro_req", bodyBytes)

	var finalTransport http.RoundTripper = c.defaultTransport
	proxyUsed := ""
	if len(proxyTransport) > 0 && proxyTransport[0] != nil {
		finalTransport = proxyTransport[0]
		proxyUsed = " [via proxy]"
	}

	client := c.getClient(finalTransport, true)

	log.Printf("[API-Stream] → %s hist=%d tools=%d%s", model, len(history), len(tools), proxyUsed)

	req, err := http.NewRequest("POST", KiroAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 用传入的 context 控制超时，客户端断开时自动取消 Kiro 请求
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req = req.WithContext(reqCtx)

	sendTime := time.Now()
	resp, err := client.Do(req)
	connectMs := time.Since(sendTime).Milliseconds()
	if err != nil {
		log.Printf("[API-Stream] ❌ 连接失败 (%dms): %v", connectMs, err)
		return nil, 0, fmt.Errorf("send request: %w", err)
	}

	if resp == nil {
		log.Printf("[API-Stream] ❌ 无响应 (%dms)", connectMs)
		return nil, 0, fmt.Errorf("no response received")
	}
	defer resp.Body.Close()

	log.Printf("[API-Stream] ← HTTP %d (%dms) Content-Type: %s", resp.StatusCode, connectMs, resp.Header.Get("Content-Type"))

	if resp.StatusCode != 200 {
		rawResp, _ := io.ReadAll(resp.Body)
		errBody := string(rawResp)
		log.Printf("[API-Stream] ❌ 错误响应 %d: %s", resp.StatusCode, TruncStr(errBody, 200))
		var errJSON map[string]interface{}
		if json.Unmarshal(rawResp, &errJSON) == nil {
			if msg, ok := errJSON["message"].(string); ok {
				return nil, resp.StatusCode, fmt.Errorf("%s", msg)
			}
			if msg, ok := errJSON["Message"].(string); ok {
				return nil, resp.StatusCode, fmt.Errorf("%s", msg)
			}
		}
		return nil, resp.StatusCode, fmt.Errorf("API error %d: %s", resp.StatusCode, TruncStr(errBody, 500))
	}

	// 直接解析 event stream
	result, err := c.parseEventStreamIncremental(resp.Body, onChunk)
	if err != nil {
		log.Printf("[API-Stream] ⚠️ 流式解析错误: %v (chunks=%d)", err, len(result.Content))
		return result, resp.StatusCode, err
	}

	log.Printf("[API-Stream] ← complete, chunks=%d credits=%.4f ctx=%.1f%%", len(result.Content), result.CreditsUsed, result.ContextUsagePercent)
	return result, resp.StatusCode, nil
}

// parseEventStreamIncremental reads AWS event-stream messages one at a time from a reader,
// calling onChunk for each text fragment as it arrives.
func (c *KiroClient) parseEventStreamIncremental(reader io.Reader, onChunk StreamCallback) (*KiroResponse, error) {
	result := &KiroResponse{}
	toolInputBuffers := make(map[string]string)
	crcTable := crc32.IEEETable
	firstFrame := true
	streamStart := time.Now()

	// Kiro 事件日志（受 debug 开关控制）
	var kiroLogFile *os.File
	if IsDebugSaveEnabled() {
		kiroLogFile, _ = os.Create(fmt.Sprintf("logs/kiro_events_%s.log", time.Now().Format("20060102_150405")))
	}
	defer func() {
		if kiroLogFile != nil {
			kiroLogFile.Close()
		}
	}()

	for {
		prelude := make([]byte, 12)
		_, err := io.ReadFull(reader, prelude)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if firstFrame {
					log.Printf("[API-Stream] ⚠️ Kiro 返回空响应 (等待 %dms 后 EOF)", time.Since(streamStart).Milliseconds())
				}
				break
			}
			log.Printf("[API-Stream] ⚠️ 读取中断: %v (已收到 %d chunks, 等待 %dms)", err, len(result.Content), time.Since(streamStart).Milliseconds())
			return result, fmt.Errorf("read prelude: %w", err)
		}

		if firstFrame {
			log.Printf("[API-Stream] 首帧到达 (等待 %dms)", time.Since(streamStart).Milliseconds())
			firstFrame = false
		}

		totalLen := binary.BigEndian.Uint32(prelude[0:4])
		headersLen := binary.BigEndian.Uint32(prelude[4:8])

		preludeCRC := binary.BigEndian.Uint32(prelude[8:12])
		computedCRC := crc32.Checksum(prelude[0:8], crcTable)
		if preludeCRC != computedCRC {
			// CRC 不匹配，可能是代理损坏了数据流，尝试重新同步
			log.Printf("[API-Stream] ⚠️ prelude CRC mismatch (got=%d want=%d)，尝试跳过损坏数据", preludeCRC, computedCRC)
			// 逐字节读取直到找到下一个有效的 prelude 或 EOF
			syncBuf := make([]byte, 1)
			synced := false
			for attempt := 0; attempt < 4096; attempt++ {
				// 把当前 prelude 往前移一字节
				copy(prelude[0:11], prelude[1:12])
				_, err := io.ReadFull(reader, syncBuf)
				if err != nil {
					return result, nil // 流结束，返回已有结果
				}
				prelude[11] = syncBuf[0]
				newCRC := crc32.Checksum(prelude[0:8], crcTable)
				newPreludeCRC := binary.BigEndian.Uint32(prelude[8:12])
				if newCRC == newPreludeCRC {
					// 找到有效 prelude
					totalLen = binary.BigEndian.Uint32(prelude[0:4])
					headersLen = binary.BigEndian.Uint32(prelude[4:8])
					synced = true
					log.Printf("[API-Stream] ✅ 重新同步成功 (跳过 %d 字节)", attempt+1)
					break
				}
			}
			if !synced {
				log.Printf("[API-Stream] ❌ 无法重新同步，返回已有结果")
				return result, nil
			}
		}

		remaining := int(totalLen) - 12
		if remaining <= 0 {
			continue
		}
		msgData := make([]byte, remaining)
		_, err = io.ReadFull(reader, msgData)
		if err != nil {
			log.Printf("[API-Stream] ⚠️ 消息体读取中断: %v (已收到 %d chunks, 帧大小=%d)", err, len(result.Content), remaining)
			// 返回已有结果而不是报错，这样已收到的内容不会丢
			return result, nil
		}

		headerBytes := msgData[:headersLen]
		payloadLen := int(totalLen) - 12 - int(headersLen) - 4
		if payloadLen < 0 {
			continue
		}
		payload := msgData[headersLen : headersLen+uint32(payloadLen)]

		eventType := ""
		pos := 0
		for pos < len(headerBytes) {
			if pos >= len(headerBytes) {
				break
			}
			nameLen := int(headerBytes[pos])
			pos++
			if pos+nameLen > len(headerBytes) {
				break
			}
			name := string(headerBytes[pos : pos+nameLen])
			pos += nameLen
			if pos >= len(headerBytes) {
				break
			}
			valueType := headerBytes[pos]
			pos++
			if valueType == 7 {
				if pos+2 > len(headerBytes) {
					break
				}
				valueLen := int(binary.BigEndian.Uint16(headerBytes[pos : pos+2]))
				pos += 2
				if pos+valueLen > len(headerBytes) {
					break
				}
				value := string(headerBytes[pos : pos+valueLen])
				pos += valueLen
				if name == ":event-type" {
					eventType = value
				}
			} else {
				break
			}
		}

		// 记录 Kiro 事件到日志
		if kiroLogFile != nil {
			fmt.Fprintf(kiroLogFile, "[%s] eventType=%s payload=%s\n", time.Now().Format("15:04:05.000"), eventType, string(payload))
		}

		switch eventType {
		case "assistantResponseEvent":
			var evt struct {
				Content string `json:"content"`
				ModelId string `json:"modelId"`
			}
			if json.Unmarshal(payload, &evt) == nil && evt.Content != "" {
				text := evt.Content
				result.Content = append(result.Content, text)
				if onChunk != nil {
					onChunk(StreamEvent{Type: "text", Text: text})
				}
			}
		case "":
		case "toolUseEvent":
			var tu struct {
				ToolUseID string  `json:"toolUseId"`
				Name      string  `json:"name"`
				Input     *string `json:"input"`
				Stop      bool    `json:"stop"`
			}
			if json.Unmarshal(payload, &tu) == nil {
				if tu.Stop {
					// tool_stop: 工具调用结束
					toolName := tu.Name
					if toolName == "" {
						toolName = toolInputBuffers[tu.ToolUseID+"_name"]
					}
					// 解析累积的 input JSON
					var inputParsed interface{}
					inputStr := toolInputBuffers[tu.ToolUseID]
					if inputStr != "" {
						json.Unmarshal([]byte(inputStr), &inputParsed)
					}
					if inputParsed == nil {
						inputParsed = map[string]interface{}{}
					}
					toolJSON, _ := json.Marshal(map[string]interface{}{
						"toolUseId": tu.ToolUseID,
						"name":      toolName,
						"input":     inputParsed,
					})
					result.ToolUses = append(result.ToolUses, json.RawMessage(toolJSON))
					onChunk(StreamEvent{Type: "tool_stop", ToolUseID: tu.ToolUseID})
				} else if tu.Input != nil {
					// tool_input: 输入片段
					toolInputBuffers[tu.ToolUseID] += *tu.Input
					onChunk(StreamEvent{Type: "tool_input", ToolUseID: tu.ToolUseID, Input: *tu.Input})
				} else if tu.Name != "" {
					// tool_start: 新工具调用开始
					toolInputBuffers[tu.ToolUseID] = ""
					toolInputBuffers[tu.ToolUseID+"_name"] = tu.Name
					onChunk(StreamEvent{Type: "tool_start", ToolUseID: tu.ToolUseID, ToolName: tu.Name})
				}
			}

		case "meteringEvent":
			var evt struct {
				Usage       float64 `json:"usage"`
				CreditsUsed float64 `json:"creditsUsed"`
			}
			if json.Unmarshal(payload, &evt) == nil {
				if evt.Usage > 0 {
					result.CreditsUsed = evt.Usage
				} else if evt.CreditsUsed > 0 {
					result.CreditsUsed = evt.CreditsUsed
				}
			}

		case "contextUsageEvent":
			var evt struct {
				ContextUsagePercentage float64 `json:"contextUsagePercentage"`
				UsagePercent           float64 `json:"usagePercent"`
			}
			if json.Unmarshal(payload, &evt) == nil {
				if evt.ContextUsagePercentage > 0 {
					result.ContextUsagePercent = evt.ContextUsagePercentage
				} else if evt.UsagePercent > 0 {
					result.ContextUsagePercent = evt.UsagePercent
				}
			}

		case "supplementaryWebLinksEvent":
			// ignore

		default:
			var generic map[string]interface{}
			if json.Unmarshal(payload, &generic) == nil {
				if sr, ok := generic["stopReason"].(string); ok && sr != "" {
					result.StopReason = sr
				}
			}
		}
	}

	if result.StopReason == "" {
		result.StopReason = "end_turn"
	}
	return result, nil
}
