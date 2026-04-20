package kiro

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type KiroClient struct {
	// defaultTransport is a shared transport reused across requests (no proxy).
	defaultTransport http.RoundTripper
	// defaultClient is a shared http.Client for non-streaming requests (no proxy).
	defaultClient *http.Client
	// streamClient is a shared http.Client for streaming requests (no proxy, no timeout).
	streamClient *http.Client
	// proxyClients caches http.Client per transport pointer, avoiding repeated allocation.
	proxyClientsMu sync.RWMutex
	proxyClients   map[http.RoundTripper]*http.Client
	proxyStreamMu  sync.RWMutex
	proxyStream    map[http.RoundTripper]*http.Client
}

// buildConversationID 基于模型和首条消息内容生成确定性 conversationId
// 同一对话的后续请求会复用同一个 ID
func buildConversationID(model, anchor string) string {
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return uuid.New().String()
	}
	seed := model + "\n" + anchor
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

// BuildConversationID 公共版本，供外部获取 Kiro conversationId
func BuildConversationID(model string, history []interface{}, userContent string) string {
	anchor := ""
	if len(history) > 0 {
		if first, ok := history[0].(map[string]interface{}); ok {
			if um, ok := first["userInputMessage"].(map[string]interface{}); ok {
				if c, ok := um["content"].(string); ok {
					anchor = c
				}
			}
		}
	}
	if anchor == "" {
		anchor = userContent
	}
	return buildConversationID(model, anchor)
}

func NewKiroClient(connectTimeoutSecs ...int) *KiroClient {
	dialTimeout := 15 * time.Second
	if len(connectTimeoutSecs) > 0 && connectTimeoutSecs[0] > 0 {
		dialTimeout = time.Duration(connectTimeoutSecs[0]) * time.Second
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second, // TCP keep-alive，防止代理/防火墙断开空闲连接
		}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout:   dialTimeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &KiroClient{
		defaultTransport: transport,
		defaultClient: &http.Client{
			Timeout:   120 * time.Second,
			Transport: transport,
		},
		streamClient: &http.Client{
			Transport: transport, // no Timeout — stream can run indefinitely
		},
		proxyClients: make(map[http.RoundTripper]*http.Client),
		proxyStream:  make(map[http.RoundTripper]*http.Client),
	}
}

// getClient 获取或创建一个 http.Client（复用，避免每次请求都 new）
func (c *KiroClient) getClient(transport http.RoundTripper, streaming bool) *http.Client {
	if transport == nil || transport == c.defaultTransport {
		if streaming {
			return c.streamClient
		}
		return c.defaultClient
	}
	if streaming {
		c.proxyStreamMu.RLock()
		if cl, ok := c.proxyStream[transport]; ok {
			c.proxyStreamMu.RUnlock()
			return cl
		}
		c.proxyStreamMu.RUnlock()
		cl := &http.Client{Transport: transport}
		c.proxyStreamMu.Lock()
		c.proxyStream[transport] = cl
		c.proxyStreamMu.Unlock()
		return cl
	}
	c.proxyClientsMu.RLock()
	if cl, ok := c.proxyClients[transport]; ok {
		c.proxyClientsMu.RUnlock()
		return cl
	}
	c.proxyClientsMu.RUnlock()
	cl := &http.Client{Timeout: 120 * time.Second, Transport: transport}
	c.proxyClientsMu.Lock()
	c.proxyClients[transport] = cl
	c.proxyClientsMu.Unlock()
	return cl
}

func (c *KiroClient) BuildHeaders(token, agentMode, machineID string) map[string]string {
	if agentMode == "" {
		agentMode = "vibe"
	}
	kiroVersion := DefaultKiroVersion

	return map[string]string{
		"content-type":                "application/json",
		"x-amzn-codewhisperer-optout": "true",
		"x-amzn-kiro-agent-mode":      agentMode,
		"x-amz-user-agent":            fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", DefaultSDKVersion, kiroVersion, machineID),
		"user-agent":                  fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/win32#10.0.19045 lang/js md/nodejs#%s api/codewhispererstreaming#%s m/E KiroIDE-%s-%s", DefaultSDKVersion, DefaultNodeVersion, DefaultSDKVersion, kiroVersion, machineID),
		"amz-sdk-invocation-id":       uuid.New().String(),
		"amz-sdk-request":             "attempt=1; max=3",
		"Authorization":               "Bearer " + token,
	}
}

type KiroRequestBody struct {
	ConversationState struct {
		AgentContinuationID string                 `json:"agentContinuationId"`
		AgentTaskType       string                 `json:"agentTaskType"`
		ChatTriggerType     string                 `json:"chatTriggerType"`
		ConversationID      string                 `json:"conversationId"`
		CurrentMessage      map[string]interface{} `json:"currentMessage"`
		History             []interface{}          `json:"history"`
	} `json:"conversationState"`
}

const MaxRequestBodyKB = 300 // Max request body size in KB (before truncation)
const MaxToolResults = 50    // Max tool results to include

func (c *KiroClient) BuildRequest(userContent, model string, history []interface{}, tools []interface{}, images []interface{}, toolResults []interface{}, profileArn ...string) map[string]interface{} {
	if userContent == "" {
		userContent = "Continue"
	}

	// Fix history alternation should be done by caller before BuildRequest
	// (use convert.FixHistoryAlternation)

	// Fallback hard truncation if history is still too large after compression
	if len(history) > MaxHistoryItems {
		history = history[len(history)-MaxHistoryItems:]
		for len(history) > 0 {
			if _, ok := history[0].(map[string]interface{})["userInputMessage"]; ok {
				break
			}
			history = history[1:]
		}
		if len(history) > 0 {
			if firstUser, ok := history[0].(map[string]interface{})["userInputMessage"].(map[string]interface{}); ok {
				if ctx, ok := firstUser["userInputMessageContext"].(map[string]interface{}); ok {
					if _, hasTR := ctx["toolResults"]; hasTR {
						delete(ctx, "toolResults")
						if len(ctx) == 0 {
							delete(firstUser, "userInputMessageContext")
						}
					}
				}
			}
		}
	}

	// CRITICAL: Fix all broken toolUse/toolResult pairs inside history.
	history = sanitizeHistoryToolPairs(history)

	// CRITICAL: Validate toolResults against last assistant's toolUses.
	toolResults = validateToolResultsAgainstHistory(toolResults, history)

	// Truncate tool results
	if len(toolResults) > MaxToolResults {
		toolResults = toolResults[len(toolResults)-MaxToolResults:]
	}

	userInputMessage := map[string]interface{}{
		"content": userContent,
		"modelId": model,
		"origin":  "AI_EDITOR",
	}

	if len(images) > 0 {
		userInputMessage["images"] = images
	}

	ctx := map[string]interface{}{}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}
	if len(toolResults) > 0 {
		ctx["toolResults"] = toolResults
	}
	if len(ctx) > 0 {
		userInputMessage["userInputMessageContext"] = ctx
	}

	// 基于内容确定性生成 conversationId，同一对话复用同一个 ID
	anchor := ""
	if len(history) > 0 {
		if first, ok := history[0].(map[string]interface{}); ok {
			if um, ok := first["userInputMessage"].(map[string]interface{}); ok {
				if c, ok := um["content"].(string); ok {
					anchor = c
				}
			}
		}
	}
	if anchor == "" {
		anchor = userContent
	}
	convID := buildConversationID(model, anchor)

	arn := ""
	if len(profileArn) > 0 {
		arn = profileArn[0]
	}

	result := map[string]interface{}{
		"conversationState": map[string]interface{}{
			"agentContinuationId": uuid.New().String(),
			"agentTaskType":       "vibe",
			"chatTriggerType":     "MANUAL",
			"conversationId":      convID,
			"currentMessage": map[string]interface{}{
				"userInputMessage": userInputMessage,
			},
			"history": func() interface{} {
				if history == nil {
					return []interface{}{}
				}
				return history
			}(),
		},
	}
	// 只有 profileArn 非空时才加入请求体
	if arn != "" {
		result["profileArn"] = arn
	}
	return result
}

func validateToolResultsAgainstHistory(toolResults []interface{}, history []interface{}) []interface{} {
	if len(toolResults) == 0 {
		return toolResults
	}

	if len(history) == 0 {
		log.Printf("[BuildRequest] ⚠️ 丢弃 %d 个 toolResults: history 为空", len(toolResults))
		return nil
	}

	var lastAssistant map[string]interface{}
	var lastAssistantIdx int = -1
	for i := len(history) - 1; i >= 0; i-- {
		entry, ok := history[i].(map[string]interface{})
		if !ok {
			continue
		}
		if arm, ok := entry["assistantResponseMessage"].(map[string]interface{}); ok {
			lastAssistant = arm
			lastAssistantIdx = i
			break
		}
	}

	if lastAssistant == nil {
		log.Printf("[BuildRequest] ⚠️ 丢弃 %d 个 toolResults: history 中没有 assistant 消息", len(toolResults))
		return nil
	}

	existingToolUses, _ := lastAssistant["toolUses"].([]interface{})

	existingIDs := make(map[string]bool)
	for _, tu := range existingToolUses {
		if tuMap, ok := tu.(map[string]interface{}); ok {
			if id, _ := tuMap["toolUseId"].(string); id != "" {
				existingIDs[id] = true
			}
		}
	}

	var needsSynthetic []interface{}
	var alreadyMatched []interface{}
	for _, tr := range toolResults {
		trMap, ok := tr.(map[string]interface{})
		if !ok {
			continue
		}
		trID, _ := trMap["toolUseId"].(string)
		if trID != "" && existingIDs[trID] {
			alreadyMatched = append(alreadyMatched, tr)
		} else {
			needsSynthetic = append(needsSynthetic, tr)
		}
	}

	if len(needsSynthetic) == 0 {
		return toolResults
	}

	var syntheticToolUses []interface{}
	for _, tr := range needsSynthetic {
		trMap, _ := tr.(map[string]interface{})
		trID, _ := trMap["toolUseId"].(string)
		if trID == "" {
			continue
		}
		syntheticToolUses = append(syntheticToolUses, map[string]interface{}{
			"toolUseId": trID,
			"name":      "unknown_tool",
			"input":     map[string]interface{}{},
		})
	}

	allToolUses := append(existingToolUses, syntheticToolUses...)
	lastAssistant["toolUses"] = allToolUses

	log.Printf("[BuildRequest] 🔧 为 %d 个孤立 toolResults 补充了占位 toolUses (assistant idx=%d, 总toolUses=%d)",
		len(syntheticToolUses), lastAssistantIdx, len(allToolUses))

	return toolResults
}

func sanitizeHistoryToolPairs(history []interface{}) []interface{} {
	if len(history) < 2 {
		return history
	}

	fixed := 0

	for i := 0; i < len(history); i++ {
		entry, ok := history[i].(map[string]interface{})
		if !ok {
			continue
		}

		if um, ok := entry["userInputMessage"].(map[string]interface{}); ok {
			ctx, _ := um["userInputMessageContext"].(map[string]interface{})
			if ctx == nil {
				continue
			}
			trRaw, hasTR := ctx["toolResults"]
			if !hasTR {
				continue
			}
			toolResults, ok := trRaw.([]interface{})
			if !ok || len(toolResults) == 0 {
				continue
			}

			if i == 0 {
				delete(ctx, "toolResults")
				if len(ctx) == 0 {
					delete(um, "userInputMessageContext")
				}
				if c, _ := um["content"].(string); c == "Tool results provided." || c == "" {
					um["content"] = "Continue"
				}
				fixed++
				continue
			}

			prevEntry, ok := history[i-1].(map[string]interface{})
			if !ok {
				continue
			}
			prevAssistant, ok := prevEntry["assistantResponseMessage"].(map[string]interface{})
			if !ok {
				delete(ctx, "toolResults")
				if len(ctx) == 0 {
					delete(um, "userInputMessageContext")
				}
				fixed++
				continue
			}

			existingTU, _ := prevAssistant["toolUses"].([]interface{})
			existingIDs := make(map[string]bool)
			for _, tu := range existingTU {
				if m, ok := tu.(map[string]interface{}); ok {
					if id, _ := m["toolUseId"].(string); id != "" {
						existingIDs[id] = true
					}
				}
			}

			var missingTUs []interface{}
			for _, tr := range toolResults {
				trMap, ok := tr.(map[string]interface{})
				if !ok {
					continue
				}
				trID, _ := trMap["toolUseId"].(string)
				if trID != "" && !existingIDs[trID] {
					missingTUs = append(missingTUs, map[string]interface{}{
						"toolUseId": trID,
						"name":      "tool_call",
						"input":     map[string]interface{}{},
					})
				}
			}

			if len(missingTUs) > 0 {
				allTU := append(existingTU, missingTUs...)
				prevAssistant["toolUses"] = allTU
				fixed++
			}
		}

		if am, ok := entry["assistantResponseMessage"].(map[string]interface{}); ok {
			tuRaw, hasTU := am["toolUses"]
			if !hasTU {
				continue
			}
			toolUses, ok := tuRaw.([]interface{})
			if !ok || len(toolUses) == 0 {
				continue
			}

			if i+1 >= len(history) {
				delete(am, "toolUses")
				fixed++
				continue
			}

			nextEntry, ok := history[i+1].(map[string]interface{})
			if !ok {
				continue
			}
			nextUser, ok := nextEntry["userInputMessage"].(map[string]interface{})
			if !ok {
				delete(am, "toolUses")
				fixed++
				continue
			}

			nextCtx, _ := nextUser["userInputMessageContext"].(map[string]interface{})
			nextTR, _ := nextCtx["toolResults"].([]interface{})
			if len(nextTR) == 0 {
				delete(am, "toolUses")
				fixed++
			}
		}
	}

	if fixed > 0 {
		log.Printf("[SanitizeHistory] 修复了 %d 处 history 内部的 toolUse/toolResult 配对问题", fixed)
	}

	return history
}

type KiroResponse struct {
	Content             []string          `json:"content"`
	ToolUses            []json.RawMessage `json:"tool_uses"`
	StopReason          string            `json:"stop_reason"`
	CreditsUsed         float64           `json:"credits_used"`
	ContextUsagePercent float64           `json:"context_usage_percent"`
}

func (c *KiroClient) ParseEventStream(raw []byte) *KiroResponse {
	result := &KiroResponse{
		StopReason: "end_turn",
	}

	toolInputBuffer := make(map[string]*toolBuf)
	pos := 0

	for pos < len(raw) {
		if pos+12 > len(raw) {
			break
		}

		totalLen := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		headersLen := int(binary.BigEndian.Uint32(raw[pos+4 : pos+8]))

		if totalLen == 0 || totalLen > len(raw)-pos {
			break
		}

		headerStart := pos + 12
		headerEnd := headerStart + headersLen
		eventType := ""
		if headerEnd <= len(raw) {
			headerData := string(raw[headerStart:headerEnd])
			if strings.Contains(headerData, "toolUseEvent") {
				eventType = "toolUseEvent"
			} else if strings.Contains(headerData, "assistantResponseEvent") {
				eventType = "assistantResponseEvent"
			} else if strings.Contains(headerData, "meteringEvent") {
				eventType = "meteringEvent"
			} else if strings.Contains(headerData, "contextUsageEvent") {
				eventType = "contextUsageEvent"
			}
		}

		payloadStart := pos + 12 + headersLen
		payloadEnd := pos + totalLen - 4

		if payloadStart < payloadEnd && payloadEnd <= len(raw) {
			payloadBytes := raw[payloadStart:payloadEnd]
			var payload map[string]interface{}
			if err := json.Unmarshal(payloadBytes, &payload); err == nil {
				if evt, ok := payload["assistantResponseEvent"].(map[string]interface{}); ok {
					if content, ok := evt["content"].(string); ok {
						result.Content = append(result.Content, content)
					}
				} else if content, ok := payload["content"].(string); ok && eventType != "toolUseEvent" {
					result.Content = append(result.Content, content)
				}

				if eventType == "toolUseEvent" || payload["toolUseId"] != nil {
					toolID, _ := payload["toolUseId"].(string)
					toolName, _ := payload["name"].(string)
					toolInput, _ := payload["input"].(string)

					if toolID != "" {
						if _, exists := toolInputBuffer[toolID]; !exists {
							toolInputBuffer[toolID] = &toolBuf{
								ID:   toolID,
								Name: toolName,
							}
						}
						if toolName != "" && toolInputBuffer[toolID].Name == "" {
							toolInputBuffer[toolID].Name = toolName
						}
						if toolInput != "" {
							toolInputBuffer[toolID].InputParts = append(toolInputBuffer[toolID].InputParts, toolInput)
						}
					}
				}

				if eventType == "meteringEvent" {
					if usage, ok := payload["usage"].(float64); ok {
						result.CreditsUsed = usage
					}
					if me, ok := payload["meteringEvent"].(map[string]interface{}); ok {
						if usage, ok := me["usage"].(float64); ok {
							result.CreditsUsed = usage
						}
					}
				}

				if eventType == "contextUsageEvent" {
					if pct, ok := payload["contextUsagePercentage"].(float64); ok {
						result.ContextUsagePercent = pct
					}
					if ce, ok := payload["contextUsageEvent"].(map[string]interface{}); ok {
						if pct, ok := ce["contextUsagePercentage"].(float64); ok {
							result.ContextUsagePercent = pct
						}
					}
				}
			}
		}

		pos += totalLen
	}

	for _, tb := range toolInputBuffer {
		inputStr := strings.Join(tb.InputParts, "")
		var inputJSON interface{}
		if err := json.Unmarshal([]byte(inputStr), &inputJSON); err != nil {
			inputJSON = map[string]string{"raw": inputStr}
		}

		toolUse := map[string]interface{}{
			"type":  "tool_use",
			"id":    tb.ID,
			"name":  tb.Name,
			"input": inputJSON,
		}
		raw, _ := json.Marshal(toolUse)
		result.ToolUses = append(result.ToolUses, raw)
	}

	if len(result.ToolUses) > 0 {
		result.StopReason = "tool_use"
	}

	return result
}

type toolBuf struct {
	ID         string
	Name       string
	InputParts []string
}

func (c *KiroClient) SendRequest(token, machineID, model, userContent string, history []interface{}, tools []interface{}, images []interface{}, toolResults []interface{}, profileArn string, proxyTransport ...http.RoundTripper) (*KiroResponse, int, error) {
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

	client := c.getClient(finalTransport, false)

	log.Printf("[API] → %s hist=%d tools=%d%s", model, len(history), len(tools), proxyUsed)

	req, err := http.NewRequest("POST", KiroAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	log.Printf("[API] ← %d (%d bytes)", resp.StatusCode, len(rawResp))

	SaveBodyToFile(fmt.Sprintf("resp_%d", resp.StatusCode), rawResp)

	if resp.StatusCode != 200 {
		errBody := string(rawResp)
		log.Printf("[API] ERROR %d response body: %s", resp.StatusCode, TruncStr(errBody, 1000))

		var errJSON map[string]interface{}
		if json.Unmarshal(rawResp, &errJSON) == nil {
			if msg, ok := errJSON["message"].(string); ok {
				log.Printf("[API] Error message: %s", msg)
				return nil, resp.StatusCode, fmt.Errorf("%s", msg)
			}
			if msg, ok := errJSON["Message"].(string); ok {
				log.Printf("[API] Error message: %s", msg)
				return nil, resp.StatusCode, fmt.Errorf("%s", msg)
			}
		}
		return nil, resp.StatusCode, fmt.Errorf("API error %d: %s", resp.StatusCode, TruncStr(errBody, 500))
	}

	result := c.ParseEventStream(rawResp)
	return result, resp.StatusCode, nil
}

func IsQuotaExceededError(statusCode int, errorText string) bool {
	if statusCode == 429 || statusCode == 503 || statusCode == 529 {
		return true
	}
	lower := strings.ToLower(errorText)
	for _, kw := range []string{"rate limit", "quota", "too many requests", "throttl"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// UsageLimitsResponse represents the Kiro getUsageLimits API response
type UsageLimitsResponse struct {
	SubscriptionType  string  `json:"subscription_type"`
	SubscriptionTitle string  `json:"subscription_title"`
	DaysUntilReset    int     `json:"days_until_reset"`
	NextDateReset     float64 `json:"next_date_reset"`

	UsageLimit    float64 `json:"usage_limit"`
	CurrentUsage  float64 `json:"current_usage"`

	FreeTrialStatus  string  `json:"free_trial_status"`
	FreeTrialUsage   float64 `json:"free_trial_usage"`
	FreeTrialLimit   float64 `json:"free_trial_limit"`
	FreeTrialExpiry  float64 `json:"free_trial_expiry"`

	DisplayName string `json:"display_name"`
	Currency    string `json:"currency"`

	RawJSON json.RawMessage `json:"raw,omitempty"`
}

// GetUsageLimits queries the Kiro API for account usage/credits info
func (c *KiroClient) GetUsageLimits(token, machineID string, proxyTransport ...http.RoundTripper) (*UsageLimitsResponse, error) {
	kiroVersion := DefaultKiroVersion

	apiURL := "https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true"

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", DefaultSDKVersion, kiroVersion, machineID))
	req.Header.Set("user-agent", fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/win32#10.0.19045 lang/js md/nodejs#%s api/codewhispererruntime#%s m/N,E KiroIDE-%s-%s", DefaultSDKVersion, DefaultNodeVersion, DefaultSDKVersion, kiroVersion, machineID))
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("amz-sdk-invocation-id", uuid.New().String())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")

	var finalTransport http.RoundTripper = c.defaultTransport
	if len(proxyTransport) > 0 && proxyTransport[0] != nil {
		finalTransport = proxyTransport[0]
	}

	client := &http.Client{Timeout: 15 * time.Second, Transport: finalTransport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, TruncStr(string(rawResp), 200))
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(rawResp, &raw); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	result := &UsageLimitsResponse{
		RawJSON: rawResp,
	}

	if sub, ok := raw["subscriptionInfo"].(map[string]interface{}); ok {
		result.SubscriptionType, _ = sub["type"].(string)
		result.SubscriptionTitle, _ = sub["subscriptionTitle"].(string)
	}

	if d, ok := raw["daysUntilReset"].(float64); ok {
		result.DaysUntilReset = int(d)
	}
	if d, ok := raw["nextDateReset"].(float64); ok {
		result.NextDateReset = d
	}

	if list, ok := raw["usageBreakdownList"].([]interface{}); ok && len(list) > 0 {
		if item, ok := list[0].(map[string]interface{}); ok {
			result.DisplayName, _ = item["displayName"].(string)
			result.Currency, _ = item["currency"].(string)

			if limit, ok := item["usageLimitWithPrecision"].(float64); ok {
				result.UsageLimit = limit
			}
			if usage, ok := item["currentUsageWithPrecision"].(float64); ok {
				result.CurrentUsage = usage
			}

			if ft, ok := item["freeTrialInfo"].(map[string]interface{}); ok {
				result.FreeTrialStatus, _ = ft["freeTrialStatus"].(string)
				if u, ok := ft["currentUsageWithPrecision"].(float64); ok {
					result.FreeTrialUsage = u
				}
				if l, ok := ft["usageLimitWithPrecision"].(float64); ok {
					result.FreeTrialLimit = l
				}
				if e, ok := ft["freeTrialExpiry"].(float64); ok {
					result.FreeTrialExpiry = e
				}
			}
		}
	}

	return result, nil
}
