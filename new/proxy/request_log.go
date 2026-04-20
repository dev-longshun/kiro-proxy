package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const requestLogBaseDir = "request_logs"

var requestLogCounter int64

// RequestLog 一次完整请求的日志记录
type RequestLog struct {
	ID             string      `json:"id"`
	Timestamp      string      `json:"timestamp"`
	Protocol       string      `json:"protocol"` // openai, anthropic, gemini
	Model          string      `json:"model"`
	APIKey         string      `json:"api_key,omitempty"` // 脱敏
	AccountEmail   string      `json:"account_email,omitempty"`
	SessionID      string      `json:"session_id,omitempty"`       // 账号粘性绑定用的会话 ID
	SessionKey     string      `json:"session_key,omitempty"`      // 缓存/上下文追踪用的会话 key
	ConversationID string      `json:"conversation_id,omitempty"`  // Kiro conversationId（完整 UUID）
	IsNewConv      bool        `json:"is_new_conv"`                // 是否是新会话（缓存全 miss）
	Request        interface{} `json:"request"`                    // 用户发送的内容
	Response       interface{} `json:"response,omitempty"`         // 返回的内容
	InputTokens    int         `json:"input_tokens"`
	OutputTokens   int         `json:"output_tokens"`
	CacheRead      int         `json:"cache_read_tokens"`
	CacheCreation  int         `json:"cache_creation_tokens"`
	DurationMs     int64       `json:"duration_ms"`
	Success        bool        `json:"success"`
	Error          string      `json:"error,omitempty"`
}

// getLogDir 获取当前分钟的日志目录 request_logs/20260404/2307/
func getLogDir() string {
	now := time.Now()
	dir := filepath.Join(requestLogBaseDir, now.Format("20060102"), now.Format("1504"))
	os.MkdirAll(dir, 0755)
	return dir
}

// maskAPIKey 脱敏 API key
func maskAPIKey(key string) string {
	if len(key) <= 10 {
		return "***"
	}
	return key[:6] + "..." + key[len(key)-4:]
}

// SaveRequestLog 保存完整的请求/响应日志
func SaveRequestLog(rl *RequestLog) {
	seq := atomic.AddInt64(&requestLogCounter, 1)
	dir := getLogDir()
	filename := fmt.Sprintf("%s_%s_%04d.json", time.Now().Format("150405"), rl.Protocol, seq%10000)
	filePath := filepath.Join(dir, filename)

	data, err := json.MarshalIndent(rl, "", "  ")
	if err != nil {
		log.Printf("[RequestLog] JSON序列化失败: %v", err)
		return
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		log.Printf("[RequestLog] 写入失败: %v", err)
	}
}

// truncateStr 截断字符串
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// extractLastUserMessage 从 messages 里提取最后一条 user 消息的文本（截断）
func extractLastUserMessage(request interface{}, maxLen int) string {
	body, ok := request.(map[string]interface{})
	if !ok {
		return ""
	}
	messages, _ := body["messages"].([]interface{})
	// 从后往前找最后一条 user
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			return truncateStr(c, maxLen)
		case []interface{}:
			for _, block := range c {
				if b, ok := block.(map[string]interface{}); ok {
					if t, ok := b["text"].(string); ok && t != "" {
						return truncateStr(t, maxLen)
					}
				}
			}
		}
	}
	return ""
}

// truncateResponse 截断响应内容
func truncateResponse(response interface{}, maxLen int) string {
	switch r := response.(type) {
	case string:
		return truncateStr(r, maxLen)
	default:
		return ""
	}
}

// ListRequestLogs 列出最近的请求日志摘要（不含完整 request/response，只有预览）
func ListRequestLogs(limit int) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, limit)

	dateDirs, err := os.ReadDir(requestLogBaseDir)
	if err != nil {
		return result
	}

	for i := len(dateDirs) - 1; i >= 0 && len(result) < limit; i-- {
		if !dateDirs[i].IsDir() {
			continue
		}
		datePath := filepath.Join(requestLogBaseDir, dateDirs[i].Name())
		minuteDirs, err := os.ReadDir(datePath)
		if err != nil {
			continue
		}

		for j := len(minuteDirs) - 1; j >= 0 && len(result) < limit; j-- {
			if !minuteDirs[j].IsDir() {
				continue
			}
			minutePath := filepath.Join(datePath, minuteDirs[j].Name())
			files, err := os.ReadDir(minutePath)
			if err != nil {
				continue
			}

			for k := len(files) - 1; k >= 0 && len(result) < limit; k-- {
				if files[k].IsDir() || filepath.Ext(files[k].Name()) != ".json" {
					continue
				}
				filePath := filepath.Join(minutePath, files[k].Name())
				data, err := os.ReadFile(filePath)
				if err != nil {
					continue
				}
				var full map[string]interface{}
				if json.Unmarshal(data, &full) != nil {
					continue
				}

				// 构建摘要：只保留元数据 + 截断的预览
				fileKey := filepath.Join(dateDirs[i].Name(), minuteDirs[j].Name(), files[k].Name())
				summary := map[string]interface{}{
					"_file":                fileKey,
					"id":                   full["id"],
					"timestamp":            full["timestamp"],
					"protocol":             full["protocol"],
					"model":                full["model"],
					"api_key":              full["api_key"],
					"account_email":        full["account_email"],
					"session_id":           full["session_id"],
					"session_key":          full["session_key"],
					"conversation_id":      full["conversation_id"],
					"is_new_conv":          full["is_new_conv"],
					"input_tokens":         full["input_tokens"],
					"output_tokens":        full["output_tokens"],
					"cache_read_tokens":    full["cache_read_tokens"],
					"cache_creation_tokens": full["cache_creation_tokens"],
					"duration_ms":          full["duration_ms"],
					"success":              full["success"],
					"error":                full["error"],
					"request_preview":      extractLastUserMessage(full["request"], 200),
					"response_preview":     truncateResponse(full["response"], 200),
				}
				result = append(result, summary)
			}
		}
	}

	return result
}

// GetRequestLogDetail 获取单条日志的完整内容
func GetRequestLogDetail(fileKey string) (map[string]interface{}, error) {
	// 安全检查：防止路径穿越
	if strings.Contains(fileKey, "..") || strings.HasPrefix(fileKey, "/") || strings.HasPrefix(fileKey, "\\") {
		return nil, fmt.Errorf("invalid file key")
	}

	filePath := filepath.Join(requestLogBaseDir, fileKey)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("file not found")
	}

	var entry map[string]interface{}
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("invalid json")
	}
	return entry, nil
}
