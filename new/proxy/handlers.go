package proxy

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"kiro-proxy/internal/httputil"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
)

// handleCountTokens handles Anthropic /v1/messages/count_tokens endpoint
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		httputil.WriteJSON(w, 405, map[string]interface{}{"error": "Method not allowed"})
		return
	}

	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	// Estimate token count (rough: 1 token per 4 chars)
	data, _ := json.Marshal(body)
	estimatedTokens := estimateTokens(string(data))

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"input_tokens": estimatedTokens,
	})
}

// handleGeminiRoute routes Gemini API requests
func (s *Server) handleGeminiRoute(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Match /v1/models/{model}:generateContent or /v1beta/models/{model}:generateContent
	if strings.Contains(path, ":generateContent") {
		modelPart := path
		modelPart = strings.TrimPrefix(modelPart, "/v1/models/")
		modelPart = strings.TrimPrefix(modelPart, "/v1beta/models/")
		modelName := strings.TrimSuffix(modelPart, ":generateContent")

		s.handleGeminiGenerateContent(w, r, modelName)
		return
	}

	// /v1/models or /v1beta/models - list models
	if strings.HasSuffix(path, "/models") || strings.HasSuffix(path, "/models/") {
		s.handleGeminiListModels(w, r)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleGeminiListModels(w http.ResponseWriter, r *http.Request) {
	models := []map[string]interface{}{
		{"name": "models/claude-sonnet-4", "displayName": "Claude Sonnet 4", "description": "Claude Sonnet 4 via Kiro"},
		{"name": "models/claude-sonnet-4.5", "displayName": "Claude Sonnet 4.5", "description": "Claude Sonnet 4.5 via Kiro"},
		{"name": "models/claude-haiku-4.5", "displayName": "Claude Haiku 4.5", "description": "Claude Haiku 4.5 via Kiro"},
		{"name": "models/claude-opus-4.5", "displayName": "Claude Opus 4.5", "description": "Claude Opus 4.5 via Kiro"},
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"models": models})
}

func (s *Server) handleGeminiGenerateContent(w http.ResponseWriter, r *http.Request, modelName string) {
	if r.Method != "POST" {
		httputil.WriteJSON(w, 405, map[string]interface{}{"error": "Method not allowed"})
		return
	}

	startTime := time.Now()
	apiKey := r.Header.Get("X-Resolved-Key")

	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	model := kiro.MapModelName(strings.Replace(modelName, "models/", "", 1))
	contents, _ := body["contents"].([]interface{})

	// Convert Gemini contents to user message + history
	var userContent string
	var history []interface{}

	// Handle system instruction
	if sysInst, ok := body["systemInstruction"].(map[string]interface{}); ok {
		if parts, ok := sysInst["parts"].([]interface{}); ok {
			var sysText string
			for _, p := range parts {
				if part, ok := p.(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok {
						sysText += text + "\n"
					}
				}
			}
			if sysText != "" {
				history = append(history, map[string]interface{}{
					"userInputMessage": map[string]interface{}{
						"content": "[System] " + sysText,
						"modelId": "claude-sonnet-4",
						"origin":  "AI_EDITOR",
					},
				})
				history = append(history, map[string]interface{}{
					"assistantResponseMessage": map[string]interface{}{
						"content": "I understand.",
					},
				})
			}
		}
	}

	// Convert contents
	for i, c := range contents {
		content, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := content["role"].(string)
		parts, _ := content["parts"].([]interface{})

		var text string
		for _, p := range parts {
			if part, ok := p.(map[string]interface{}); ok {
				if t, ok := part["text"].(string); ok {
					text += t
				}
			}
		}

		if role == "user" {
			if i == len(contents)-1 {
				userContent = text
			} else {
				history = append(history, map[string]interface{}{
					"userInputMessage": map[string]interface{}{
						"content": text,
						"modelId": "claude-sonnet-4",
						"origin":  "AI_EDITOR",
					},
				})
			}
		} else if role == "model" {
			history = append(history, map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": text,
				},
			})
		}
	}

	if userContent == "" {
		userContent = "Continue"
	}

	// 历史压缩已禁用

	// Estimate input tokens
	inputTokens, _ := legacy.EstimateTokens(body, "")

	acc := s.accountMgr.GetNextAccount("")
	if acc == nil {
		httputil.WriteJSON(w, 503, map[string]interface{}{"error": "No available accounts"})
		return
	}

	log.Printf("[Gemini] model=%s account=%s", model, acc.Email)

	result, statusCode, actualAcc, err := s.sendWithRetry(acc, model, userContent, history, nil, nil, nil)
	if actualAcc != nil {
		acc = actualAcc
	}
	if err != nil {
		// Record failed usage
		s.usageTracker.RecordUsage(legacy.UsageRecord{
			Timestamp:    time.Now(),
			APIKey:       apiKey,
			Model:        model,
			Protocol:     "gemini",
			AccountEmail: acc.Email,
			InputTokens:  inputTokens,
			OutputTokens: 0,
			TotalTokens:  inputTokens,
			Success:      false,
			DurationMs:   time.Since(startTime).Milliseconds(),
		})
		kiroErr := kiro.ClassifyError(statusCode, err.Error())
		httputil.WriteJSON(w, statusCode, map[string]interface{}{
			"error": map[string]interface{}{
				"code":    statusCode,
				"message": kiroErr.UserMessage,
				"status":  string(kiroErr.Type),
			},
		})
		return
	}

	content := strings.Join(result.Content, "")
	_, outputTokens := legacy.EstimateTokens(nil, content)

	// Record successful usage
	s.usageTracker.RecordUsage(legacy.UsageRecord{
		Timestamp:    time.Now(),
		APIKey:       apiKey,
		Model:        model,
		Protocol:     "gemini",
		AccountEmail: acc.Email,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Success:      true,
		DurationMs:   time.Since(startTime).Milliseconds(),
	})

	// Convert to Gemini response format
	var parts []map[string]interface{}
	if content != "" {
		parts = append(parts, map[string]interface{}{"text": content})
	}

	// Add tool calls as function call parts
	for _, raw := range result.ToolUses {
		var tu map[string]interface{}
		json.Unmarshal(raw, &tu)
		parts = append(parts, map[string]interface{}{
			"functionCall": map[string]interface{}{
				"name": tu["name"],
				"args": tu["input"],
			},
		})
	}

	finishReason := "STOP"
	if len(result.ToolUses) > 0 {
		finishReason = "FUNCTION_CALL"
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"candidates": []map[string]interface{}{
			{
				"content": map[string]interface{}{
					"parts": parts,
					"role":  "model",
				},
				"finishReason": finishReason,
			},
		},
		"modelVersion": model,
		"usageMetadata": map[string]interface{}{
			"promptTokenCount":     inputTokens,
			"candidatesTokenCount": outputTokens,
			"totalTokenCount":      inputTokens + outputTokens,
		},
	})
}

// handleAccountAction routes dynamic account actions
func (s *Server) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// Remove prefix: /api/accounts/
	path = strings.TrimPrefix(path, "/api/accounts/")

	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	accountID := parts[0]

	if len(parts) == 1 {
		// /api/accounts/{id} - DELETE
		s.handleDeleteAccount(w, r, accountID)
		return
	}

	action := parts[1]
	switch action {
	case "toggle":
		s.handleToggleAccount(w, r, accountID)
	case "refresh":
		s.handleRefreshAccount(w, r, accountID)
	case "restore":
		s.handleRestoreAccount(w, r, accountID)
	case "bind-proxy":
		s.handleBindProxy(w, r, accountID)
	case "unbind-proxy":
		s.handleUnbindProxy(w, r, accountID)
	case "usage-limits":
		s.handleAccountUsageLimits(w, r, accountID)
	case "edit":
		s.handleEditAccount(w, r, accountID)
	default:
		http.NotFound(w, r)
	}
}
