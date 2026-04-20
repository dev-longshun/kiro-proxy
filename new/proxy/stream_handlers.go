package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"kiro-proxy/internal/kiro"
)

// StreamOpenAIFromKiro sends a Kiro request and streams the response as OpenAI SSE in real-time.
// Returns the final KiroResponse for usage tracking.
func StreamOpenAIFromKiro(
	w http.ResponseWriter,
	r *http.Request,
	client *kiro.KiroClient,
	token, machineID, model, userContent string,
	history, tools, images, toolResults []interface{},
	profileArn string,
	requestModel string,
	estimatedInputTokens int,
	cache CacheResult,
	proxyTransport ...http.RoundTripper,
) (*kiro.KiroResponse, int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, 0, fmt.Errorf("streaming not supported")
	}

	chatID := "chatcmpl-" + uuid.New().String()[:8]
	created := time.Now().Unix()
	headerSent := false

	// Stream content chunks as they arrive from Kiro
	onChunk := func(event kiro.StreamEvent) {
		if !headerSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)

			roleChunk := map[string]interface{}{
				"id": chatID, "object": "chat.completion.chunk", "created": created, "model": requestModel,
				"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil}},
			}
			data, _ := json.Marshal(roleChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			headerSent = true
		}

		if event.Type == "text" {
			chunk := map[string]interface{}{
				"id": chatID, "object": "chat.completion.chunk", "created": created, "model": requestModel,
				"choices": []map[string]interface{}{{"index": 0, "delta": map[string]string{"content": event.Text}, "finish_reason": nil}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
		// OpenAI 格式的 tool_calls 在 finish 时一次性发送
	}

	result, statusCode, err := client.SendRequestStream(
		r.Context(),
		token, machineID, model, userContent,
		history, tools, images, toolResults,
		profileArn, onChunk, proxyTransport...,
	)
	if err != nil {
		if !headerSent {
			// Header 还没发，调用方可以重试
			return nil, statusCode, err
		}
		// Header 已发，只能通过 SSE error event 通知客户端
		errChunk := map[string]interface{}{
			"error": map[string]interface{}{
				"message": err.Error(),
				"type":    "server_error",
			},
		}
		data, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return nil, statusCode, err
	}

	// 如果 Kiro 返回了 tool_use 但没有文本 content，header 可能还没发
	if !headerSent {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)

		roleChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   requestModel,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
			},
		}
		data, _ := json.Marshal(roleChunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		headerSent = true
	}

	// Send tool_calls if any
	if len(result.ToolUses) > 0 {
		var toolCalls []map[string]interface{}
		for i, raw := range result.ToolUses {
			var tu map[string]interface{}
			json.Unmarshal(raw, &tu)
			inputJSON, _ := json.Marshal(tu["input"])
			toolID, _ := tu["toolUseId"].(string)
			toolName, _ := tu["name"].(string)
			if toolID == "" {
				toolID = "call_" + uuid.New().String()[:12]
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"index": i,
				"id":    toolID,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      toolName,
					"arguments": string(inputJSON),
				},
			})
		}
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   requestModel,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]interface{}{"tool_calls": toolCalls}, "finish_reason": nil},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Send finish chunk with usage
	finishReason := "stop"
	if len(result.ToolUses) > 0 {
		finishReason = "tool_calls"
	}

	outputContent := strings.Join(result.Content, "")
	outputTokens := estimateTokens(outputContent)
	for _, raw := range result.ToolUses {
		outputTokens += estimateTokens(string(raw))
	}

	finishChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   requestModel,
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{}, "finish_reason": finishReason},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     estimatedInputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      estimatedInputTokens + outputTokens,
			"prompt_tokens_details": map[string]interface{}{
				"cached_tokens": cache.ReadTokens,
				"audio_tokens":  0,
			},
			"completion_tokens_details": map[string]interface{}{
				"reasoning_tokens":             0,
				"audio_tokens":                 0,
				"accepted_prediction_tokens":   0,
				"rejected_prediction_tokens":   0,
			},
		},
	}
	finishData, _ := json.Marshal(finishChunk)
	fmt.Fprintf(w, "data: %s\n\n", finishData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	return result, statusCode, nil
}

// StreamAnthropicFromKiro sends a Kiro request and streams as Anthropic SSE in real-time.
func StreamAnthropicFromKiro(
	w http.ResponseWriter,
	r *http.Request,
	client *kiro.KiroClient,
	token, machineID, model, userContent string,
	history, tools, images, toolResults []interface{},
	profileArn string,
	requestModel string,
	estimatedInputTokens int,
	cache CacheResult,
	proxyTransport ...http.RoundTripper,
) (*kiro.KiroResponse, int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, 0, fmt.Errorf("streaming not supported")
	}

	// SSE 日志：记录所有发给客户端的 SSE 事件（受 debug 开关控制）
	var sseLogFile *os.File
	if kiro.IsDebugSaveEnabled() {
		sseLogFile, _ = os.Create(fmt.Sprintf("logs/sse_anthropic_%s.log", time.Now().Format("20060102_150405")))
	}
	writeSSE := func(format string, args ...interface{}) {
		line := fmt.Sprintf(format, args...)
		fmt.Fprint(w, line)
		if sseLogFile != nil {
			sseLogFile.WriteString(line)
		}
	}
	defer func() {
		if sseLogFile != nil {
			sseLogFile.Close()
		}
	}()

	headerSent := false

	// Anthropic: input_tokens 不包含缓存部分，三者互斥
	anthropicInputTokens := estimatedInputTokens - cache.ReadTokens - cache.CreationTokens
	if anthropicInputTokens < 0 {
		anthropicInputTokens = 0
	}

	sendHeader := func() {
		if headerSent {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)

		// message_start
		msgStart := map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            "msg_" + uuid.New().String()[:8],
				"type":          "message",
				"role":          "assistant",
				"model":         requestModel,
				"content":       []interface{}{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]int{
					"input_tokens":                anthropicInputTokens,
					"output_tokens":               0,
					"cache_creation_input_tokens": cache.CreationTokens,
					"cache_read_input_tokens":     cache.ReadTokens,
				},
			},
		}
		data, _ := json.Marshal(msgStart)
		writeSSE("event: message_start\ndata: %s\n\n", data)
		flusher.Flush()

		// ping event (Claude API sends this)
		writeSSE("event: ping\ndata: {\"type\":\"ping\"}\n\n")
		flusher.Flush()

		headerSent = true
	}

	// Stream content deltas and tool_use events
	contentIdx := 0
	textBlockStarted := false
	onChunk := func(event kiro.StreamEvent) {
		sendHeader()
		switch event.Type {
		case "text":
			if !textBlockStarted {
				bs := map[string]interface{}{"type": "content_block_start", "index": contentIdx, "content_block": map[string]string{"type": "text", "text": ""}}
				d, _ := json.Marshal(bs)
				writeSSE("event: content_block_start\ndata: %s\n\n", d)
				flusher.Flush()
				textBlockStarted = true
			}
			delta := map[string]interface{}{"type": "content_block_delta", "index": contentIdx, "delta": map[string]string{"type": "text_delta", "text": event.Text}}
			d, _ := json.Marshal(delta)
			writeSSE("event: content_block_delta\ndata: %s\n\n", d)
			flusher.Flush()
		case "tool_start":
			if textBlockStarted {
				bs := map[string]interface{}{"type": "content_block_stop", "index": contentIdx}
				d, _ := json.Marshal(bs)
				writeSSE("event: content_block_stop\ndata: %s\n\n", d)
				flusher.Flush()
				contentIdx++
				textBlockStarted = false
			}
			toolID := event.ToolUseID
			// 直接用 Kiro 返回的原始 ID，不加前缀
			bs := map[string]interface{}{"type": "content_block_start", "index": contentIdx, "content_block": map[string]interface{}{"type": "tool_use", "id": toolID, "name": event.ToolName, "input": map[string]interface{}{}}}
			d, _ := json.Marshal(bs)
			writeSSE("event: content_block_start\ndata: %s\n\n", d)
			flusher.Flush()
		case "tool_input":
			delta := map[string]interface{}{"type": "content_block_delta", "index": contentIdx, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": event.Input}}
			d, _ := json.Marshal(delta)
			writeSSE("event: content_block_delta\ndata: %s\n\n", d)
			flusher.Flush()
		case "tool_stop":
			bs := map[string]interface{}{"type": "content_block_stop", "index": contentIdx}
			d, _ := json.Marshal(bs)
			writeSSE("event: content_block_stop\ndata: %s\n\n", d)
			flusher.Flush()
			contentIdx++
		}
	}

	result, statusCode, err := client.SendRequestStream(
		r.Context(),
		token, machineID, model, userContent,
		history, tools, images, toolResults,
		profileArn, onChunk, proxyTransport...,
	)
	if err != nil {
		if !headerSent {
			// Header 还没发，调用方可以重试
			return nil, statusCode, err
		}
		// Header 已发，通过 SSE error event 通知
		errEvt := map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    "server_error",
				"message": err.Error(),
			},
		}
		data, _ := json.Marshal(errEvt)
		writeSSE("event: error\ndata: %s\n\n", data)
		flusher.Flush()
		return nil, statusCode, err
	}

	// 确保 header 已发（tool_use 场景可能没有文本 chunk）
	sendHeader()

	// 关闭最后一个 text block（如果还开着）
	if textBlockStarted {
		bs := map[string]interface{}{"type": "content_block_stop", "index": contentIdx}
		d, _ := json.Marshal(bs)
		writeSSE("event: content_block_stop\ndata: %s\n\n", d)
		flusher.Flush()
	}

	// message_delta
	stopReason := "end_turn"
	if len(result.ToolUses) > 0 {
		stopReason = "tool_use"
	}
	anthropicOutputContent := strings.Join(result.Content, "")
	anthropicOutputTokens := estimateTokens(anthropicOutputContent)
	for _, raw := range result.ToolUses {
		anthropicOutputTokens += estimateTokens(string(raw))
	}
	msgDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{
			"input_tokens":                anthropicInputTokens,
			"output_tokens":               anthropicOutputTokens,
			"cache_creation_input_tokens": cache.CreationTokens,
			"cache_read_input_tokens":     cache.ReadTokens,
		},
	}
	deltaData, _ := json.Marshal(msgDelta)
	writeSSE("event: message_delta\ndata: %s\n\n", deltaData)
	flusher.Flush()

	// message_stop
	msgStop := map[string]interface{}{"type": "message_stop"}
	stopData, _ := json.Marshal(msgStop)
	writeSSE("event: message_stop\ndata: %s\n\n", stopData)
	flusher.Flush()

	return result, statusCode, nil
}
