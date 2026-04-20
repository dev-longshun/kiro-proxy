package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AutoReply 自动回复规则
type AutoReply struct {
	Keyword  string `json:"keyword"`  // 匹配关键词（包含匹配）
	Reply    string `json:"reply"`    // 自定义回复内容
	Exact    bool   `json:"exact"`    // 是否精确匹配（默认包含匹配）
	Enabled  bool   `json:"enabled"`  // 是否启用
}

// AutoReplyManager 管理自动回复规则
type AutoReplyManager struct {
	mu       sync.RWMutex
	rules    []AutoReply
	filePath string
}

func NewAutoReplyManager(filePath string) *AutoReplyManager {
	m := &AutoReplyManager{filePath: filePath}
	m.Load()
	return m
}

// Load 从 JSON 文件加载规则
func (m *AutoReplyManager) Load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		m.rules = []AutoReply{}
		return
	}
	var rules []AutoReply
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Printf("[AutoReply] 加载规则失败: %v", err)
		m.rules = []AutoReply{}
		return
	}
	m.rules = rules
	log.Printf("[AutoReply] 加载了 %d 条自动回复规则", len(rules))
}

// Save 保存规则到 JSON 文件
func (m *AutoReplyManager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, err := json.MarshalIndent(m.rules, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.filePath, data, 0644)
}

// Match 检查用户消息是否匹配某条规则，返回回复内容
func (m *AutoReplyManager) Match(userContent string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lower := strings.ToLower(userContent)
	for _, rule := range m.rules {
		if !rule.Enabled {
			continue
		}
		keyword := strings.ToLower(rule.Keyword)
		if rule.Exact {
			if lower == keyword {
				return rule.Reply, true
			}
		} else {
			if strings.Contains(lower, keyword) {
				return rule.Reply, true
			}
		}
	}
	return "", false
}

// GetRules 获取所有规则
func (m *AutoReplyManager) GetRules() []AutoReply {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make([]AutoReply, len(m.rules))
	copy(cp, m.rules)
	return cp
}

// SetRules 设置所有规则
func (m *AutoReplyManager) SetRules(rules []AutoReply) {
	m.mu.Lock()
	m.rules = rules
	m.mu.Unlock()
	m.Save()
}

// AddRule 添加一条规则
func (m *AutoReplyManager) AddRule(rule AutoReply) {
	m.mu.Lock()
	m.rules = append(m.rules, rule)
	m.mu.Unlock()
	m.Save()
}

// DeleteRule 删除一条规则（按索引）
func (m *AutoReplyManager) DeleteRule(index int) {
	m.mu.Lock()
	if index >= 0 && index < len(m.rules) {
		m.rules = append(m.rules[:index], m.rules[index+1:]...)
	}
	m.mu.Unlock()
	m.Save()
}

// SendAutoReplySSE 以 Anthropic 流式格式发送自动回复
func SendAutoReplySSE(w http.ResponseWriter, reply, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	msgID := fmt.Sprintf("msg_auto_%d", time.Now().UnixNano())

	// message_start
	fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	}))
	w.(http.Flusher).Flush()

	// ping
	fmt.Fprintf(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	w.(http.Flusher).Flush()

	// content_block_start
	fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}))
	w.(http.Flusher).Flush()

	// content_block_delta — 分块发送
	chunks := splitReply(reply, 20)
	for _, chunk := range chunks {
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", mustJSON(map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
		}))
		w.(http.Flusher).Flush()
	}

	// content_block_stop
	fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	w.(http.Flusher).Flush()

	// message_delta
	outTokens := estimateTokens(reply)
	fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", mustJSON(map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": outTokens},
	}))
	w.(http.Flusher).Flush()

	// message_stop
	fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	w.(http.Flusher).Flush()

	log.Printf("[AutoReply] 自动回复: %s", kiro_TruncStr(reply, 50))
}

// SendAutoReplyJSON 以 Anthropic 非流式格式发送自动回复
func SendAutoReplyJSON(w http.ResponseWriter, reply, model string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": fmt.Sprintf("msg_auto_%d", time.Now().UnixNano()),
		"type": "message", "role": "assistant", "model": model,
		"content": []map[string]interface{}{{"type": "text", "text": reply}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": estimateTokens(reply)},
	})
}

// SendAutoReplyOpenAISSE 以 OpenAI 流式格式发送自动回复
func SendAutoReplyOpenAISSE(w http.ResponseWriter, reply, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	chatID := fmt.Sprintf("chatcmpl-auto-%d", time.Now().UnixNano())

	chunks := splitReply(reply, 20)
	for _, chunk := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "model": model,
			"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{"content": chunk}, "finish_reason": nil}},
		}))
		w.(http.Flusher).Flush()
	}

	// finish
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
		"id": chatID, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"}},
		"usage": map[string]interface{}{"prompt_tokens": 0, "completion_tokens": estimateTokens(reply), "total_tokens": estimateTokens(reply)},
	}))
	w.(http.Flusher).Flush()

	fmt.Fprintf(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func splitReply(text string, chunkSize int) []string {
	runes := []rune(text)
	var chunks []string
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	if len(chunks) == 0 {
		chunks = []string{text}
	}
	return chunks
}

func kiro_TruncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
