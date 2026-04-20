package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"kiro-proxy/internal/convert"
	"kiro-proxy/internal/httputil"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
)

type Server struct {
	accountMgr     *AccountManager
	kiroClient     *kiro.KiroClient
	usageTracker   *legacy.UsageTracker
	historyMgr     *kiro.HistoryManager
	proxyMgr       *legacy.ProxyManager
	keyMgr         *legacy.KeyManager
	rateLimiter    *legacy.RateLimiter
	sessionTracker *SessionTracker
	metrics        *MetricsTracker
	DB             *legacy.Database
	AutoReply      *AutoReplyManager
	EnableCache    bool // prompt caching 开关
	port           int
	apiKey         string
}

func NewServer(accountMgr *AccountManager, port int, apiKey string, usageTracker *legacy.UsageTracker, proxyMgr *legacy.ProxyManager, keyMgr *legacy.KeyManager, rateLimiter *legacy.RateLimiter) *Server {
	return &Server{
		accountMgr:     accountMgr,
		kiroClient:     kiro.NewKiroClient(rateLimiter.GetConfig().ConnectTimeoutSeconds),
		usageTracker:   usageTracker,
		historyMgr:     kiro.NewHistoryManager(),
		sessionTracker: NewSessionTracker(),
		metrics:        NewMetricsTracker(),
		AutoReply:      NewAutoReplyManager("auto_reply.json"),
		proxyMgr:     proxyMgr,
		keyMgr:       keyMgr,
		rateLimiter:  rateLimiter,
		port:         port,
		apiKey:       apiKey,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Root / status
	mux.HandleFunc("/", s.handleRoot)

	// OpenAI compatible endpoints
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)

	// Anthropic compatible endpoints
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleCountTokens)

	// Gemini compatible endpoints
	mux.HandleFunc("/v1/models/", s.handleGeminiRoute)
	mux.HandleFunc("/v1beta/models/", s.handleGeminiRoute)

	// Admin API
	mux.HandleFunc("/api/accounts", s.handleAccounts)
	mux.HandleFunc("/api/accounts/refresh-all", s.handleRefreshAll)
	mux.HandleFunc("/api/accounts/export", s.handleExportAccounts)
	mux.HandleFunc("/api/accounts/import", s.handleImportAccounts)
	mux.HandleFunc("/api/accounts/status", s.handleAccountsStatus)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/metrics", s.handleMetrics)
	mux.HandleFunc("/api/debug-save", s.handleDebugSave)
	mux.HandleFunc("/api/cache-toggle", s.handleCacheToggle)
	mux.HandleFunc("/token/status", s.handleTokenStatus)

	// Proxy management
	mux.HandleFunc("/api/proxies", s.handleProxies)
	mux.HandleFunc("/api/proxies/", s.handleProxyAction)

	// Key management
	mux.HandleFunc("/api/keys", s.handleKeys)
	mux.HandleFunc("/api/keys/", s.handleKeyAction)

	// Usage & stats
	mux.HandleFunc("/api/usage", s.handleUsage)
	mux.HandleFunc("/api/usage/records", s.handleUsageRecords)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/stats", s.handleStats)

	// Model-account mapping
	mux.HandleFunc("/api/model-accounts", s.handleModelAccounts)
	mux.HandleFunc("/api/model-accounts/", s.handleModelAccountsAction)
	mux.HandleFunc("/api/auto-reply", s.handleAutoReply)

	// Pre-proxy (chain proxy)
	mux.HandleFunc("/api/pre-proxy", s.handlePreProxy)

	// Settings
	mux.HandleFunc("/api/settings/ratelimit", s.handleRateLimitSettings)
	mux.HandleFunc("/api/settings/concurrent", s.handleConcurrentSettings)
	mux.HandleFunc("/api/settings/model-strip", s.handleModelStripSettings)
	mux.HandleFunc("/api/settings/kiro-models", s.handleKiroModelsSettings)
	mux.HandleFunc("/api/settings/model-aliases", s.handleModelAliasesSettings)

	// Web UI - Admin Dashboard
	mux.HandleFunc("/admin", s.handleAdminUI)
	mux.HandleFunc("/admin/", s.handleAdminUI)

	// Event logging (no-op)
	mux.HandleFunc("/v1/event-logging/batch", s.handleEventLogging)

	// Dynamic account routes
	mux.HandleFunc("/api/accounts/", s.handleAccountAction)

	return httputil.CORSMiddleware(s.recoveryMiddleware(s.authMiddleware(mux)))
}

// recoveryMiddleware 防止 handler panic 导致连接中断
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[Recovery] panic: %v | %s %s", err, r.Method, r.URL.Path)
				http.Error(w, "Internal Server Error", 500)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authMiddleware validates API key - supports both legacy single key and managed keys
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for root path (health check) and admin UI
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/admin") {
			next.ServeHTTP(w, r)
			return
		}

		// API management endpoints require the same API key
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if s.apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}
			key := s.extractKey(r)
			if secureCompare(key, s.apiKey) {
				next.ServeHTTP(w, r)
				return
			}
			// Allow requests from admin UI (same-origin referer)
			referer := r.Header.Get("Referer")
			if referer != "" && strings.Contains(referer, "/admin") {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": "Unauthorized: valid API key required for management endpoints",
			})
			return
		}

		// Skip auth for event logging
		if r.URL.Path == "/v1/event-logging/batch" {
			next.ServeHTTP(w, r)
			return
		}

		// If no auth configured, allow everything
		if s.apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Auth is required — extract key
		key := s.extractKey(r)

		// Check against configured API key
		if secureCompare(key, s.apiKey) {
			r.Header.Set("X-Resolved-Key", key)
			next.ServeHTTP(w, r)
			return
		}

		log.Printf("[Auth] Unauthorized request to %s from %s", r.URL.Path, r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Invalid API key. Provide via Authorization: Bearer <key> or x-api-key header",
				"type":    "authentication_error",
			},
		})
	})
}

// extractKey extracts API key from request headers/params
func (s *Server) extractKey(r *http.Request) string {
	// 1. Authorization: Bearer <key>
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// 2. x-api-key: <key>
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	// 3. ?key=<key> query parameter
	return r.URL.Query().Get("key")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	accounts := s.accountMgr.GetAllAccounts()
	active := 0
	for _, acc := range accounts {
		if acc.IsAvailable() {
			active++
		}
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"status":  "ok",
		"service": "Kiro API Proxy (Go)",
		"accounts": map[string]interface{}{
			"total":  len(accounts),
			"active": active,
		},
		"endpoints": map[string]interface{}{
			"openai_chat":    "/v1/chat/completions",
			"anthropic_chat": "/v1/messages",
			"models":         "/v1/models",
			"admin":          "/admin",
		},
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := kiro.KiroModels
	data := make([]map[string]interface{}, len(models))
	for i, m := range models {
		data[i] = map[string]interface{}{"id": m, "object": "model", "owned_by": "kiro"}
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		httputil.WriteJSON(w, 405, map[string]interface{}{"error": "Method not allowed"})
		return
	}

	startTime := time.Now()
	apiKey := r.Header.Get("X-Resolved-Key")

	// Read request body (limit to 10MB)
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Failed to read body"})
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	messages, _ := body["messages"].([]interface{})
	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	tools, _ := body["tools"].([]interface{})

	if len(messages) == 0 {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "messages is required"})
		return
	}

	// 自动回复拦截
	if reply, matched := s.checkAutoReply(messages); matched {
		stream, _ := body["stream"].(bool)
		if stream {
			SendAutoReplyOpenAISSE(w, reply, model)
		} else {
			httputil.WriteJSON(w, 200, map[string]interface{}{
				"id": fmt.Sprintf("chatcmpl-auto-%d", time.Now().UnixNano()),
				"object": "chat.completion", "model": model,
				"choices": []map[string]interface{}{{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
				"usage": map[string]interface{}{"prompt_tokens": 0, "completion_tokens": estimateTokens(reply), "total_tokens": estimateTokens(reply)},
			})
		}
		return
	}

	originalModel := model
	model = kiro.MapModelName(model)
	if model != originalModel {
		log.Printf("[OpenAI] 模型映射: %s → %s", originalModel, model)
	}

	sessionID := convert.GenerateSessionID(messages)
	acc := s.accountMgr.GetNextAccountWait(r.Context(), sessionID, model)
	if acc == nil {
		httputil.WriteJSON(w, 503, map[string]interface{}{"error": "No available accounts"})
		return
	}

	// 安全网：确保 slot 最终被释放
	openaiSlotReleased := false
	defer func() {
		if !openaiSlotReleased {
			log.Printf("[OpenAI] ⚠️ 安全网释放 slot: %s (active=%d)", acc.Email, atomic.LoadInt64(&acc.ActiveRequests))
			acc.ReleaseSlot()
		}
	}()

	if originalModel != model {
		log.Printf("[OpenAI] %s (原始: %s) → %s | msgs=%d", model, originalModel, acc.Email, len(messages))
	} else {
		log.Printf("[OpenAI] %s → %s | msgs=%d", model, acc.Email, len(messages))
	}

	userContent, history, kiroTools, images, toolResults := convert.ConvertOpenAIMessagesToKiro(messages, tools)

	// 提取会话标识
	sessionKey := extractSessionKey(r, body, messages)

	// 上下文续期：当 ctx >= 85% 时，生成摘要 + 开新会话
	ctxUsage := s.sessionTracker.GetContextUsage(sessionKey)
	if ctxUsage >= 85.0 && len(history) > 5 {
		summaryHistory := history
		if len(history) > 6 {
			summaryHistory = history[:len(history)-3]
		}
		summary := s.historyMgr.SummarizeForRenewal(summaryHistory)

		recentHistory := history
		if len(history) > 3 {
			recentHistory = history[len(history)-3:]
		}

		var newHistory []interface{}
		if summary != "" {
			newHistory = append(newHistory, map[string]interface{}{
				"userInputMessage": map[string]interface{}{
					"content": "[对话摘要]\n" + summary,
					"modelId": model,
					"origin":  "AI_EDITOR",
				},
			})
			newHistory = append(newHistory, map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": "我已了解之前的对话内容，请继续。",
				},
			})
		}
		newHistory = append(newHistory, recentHistory...)

		log.Printf("[OpenAI] 🔄 上下文 %.1f%% → 开新会话: %d 条历史 → 摘要(%d字) + %d 条最近历史",
			ctxUsage, len(history), len(summary), len(recentHistory))

		if newHistJSON, err := json.Marshal(newHistory); err == nil {
			kiro.SaveBodyToFile("renewal_new_history", newHistJSON)
		}

		history = newHistory
		s.sessionTracker.RenewSession(sessionKey)
		s.sessionTracker.SetSummary(sessionKey, summary)
	} else if ctxUsage > 70.0 && s.historyMgr.ShouldCompress(history) {
		beforeLen := len(history)
		history = s.historyMgr.CompressHistory(history, model)
		log.Printf("[OpenAI] ⚠️ 上下文 %.1f%% 超过阈值，压缩历史: %d → %d 条 (session=%s)",
			ctxUsage, beforeLen, len(history), kiro.TruncStr(sessionKey, 20))
	}

	log.Printf("[OpenAI] 转换: msgs=%d→hist=%d tools=%d ctx=%.1f%% session=%s",
		len(messages), len(history), len(kiroTools), ctxUsage, kiro.TruncStr(sessionKey, 20))

	// Estimate input tokens（只估算 messages 部分，base 单独算）
	openaiHashes := ComputeContentHash(body)
	openaiMsgHashes := ComputeMessageHashes(messages)
	inputTokens := openaiHashes.BaseTokens
	for _, mh := range openaiMsgHashes {
		inputTokens += mh.Tokens
	}

	// Prompt caching: 逐条 message hash 匹配
	openaiCache := CacheResult{}
	if s.EnableCache {
		openaiCache = s.sessionTracker.GetCachedTokensPerMessage(openaiMsgHashes, openaiHashes)
	}
	log.Printf("[OpenAI] 缓存: session=%s input=%d read=%d creation=%d base=%d enabled=%v", kiro.TruncStr(sessionKey, 30), inputTokens, openaiCache.ReadTokens, openaiCache.CreationTokens, openaiHashes.BaseTokens, s.EnableCache)

	if stream {
		// 流式请求带重试：header 未发送前持续重试，直到超时
		var streamResult *kiro.KiroResponse
		var streamErr error
		retryCfg := legacy.GlobalRateLimiter.GetConfig()
		maxAttempts := retryCfg.RetryMaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 100
		}
		timeoutSecs := retryCfg.RetryTimeoutSeconds
		if timeoutSecs <= 0 {
			timeoutSecs = 60
		}
		retryDeadline := time.Now().Add(time.Duration(timeoutSecs) * time.Second)
		attempt := 0

		for attempt < maxAttempts && time.Now().Before(retryDeadline) {
			select {
			case <-r.Context().Done():
				log.Printf("[流式重试] 客户端已断开，停止重试")
				goto streamDone
			default:
			}

			// 获取当前账号的 profileArn
			profileArn := ""
			if acc.Credentials != nil {
				profileArn = acc.Credentials.ProfileArn
			}

			var streamStatus int
			streamResult, streamStatus, streamErr = StreamOpenAIFromKiro(
				w, r, s.kiroClient, acc.GetToken(), acc.GetMachineID(),
				model, userContent, history, kiroTools, images, toolResults,
				profileArn, model, inputTokens, openaiCache, s.proxyMgr.GetTransportForAccount(acc.ID),
			)
			if streamErr == nil {
				acc.SetRequestStatus("streaming")
				break
			}

			// 判断是否可重试（429/5xx/网络错误 且 header 还没发）
			isRetryable := streamStatus == 429 || streamStatus >= 500 || streamStatus == 0 || strings.Contains(streamErr.Error(), "high traffic") || strings.Contains(streamErr.Error(), "EOF")
			if !isRetryable {
				break
			}

			attempt++

			if streamStatus == 429 {
				// 429: same account, don't release slot
				delay := time.Duration(retryCfg.Retry429DelaySeconds * float64(time.Second))
				remaining := int(time.Until(retryDeadline).Seconds())
				log.Printf("[流式重试] 账号 %s 429，立即重试 #%d/%d (剩余 %ds)",
					acc.Email, attempt, maxAttempts, remaining)
				if delay > 0 {
					select {
					case <-time.After(delay):
					case <-r.Context().Done():
						goto streamDone
					}
				}
			} else {
				// Other error: release old slot, switch account
				if streamStatus != 0 {
					acc.RecordError(streamStatus, streamErr.Error())
				}
				delay := time.Duration(retryCfg.RetryErrorDelaySeconds * float64(time.Second))
				if delay <= 0 {
					delay = 1 * time.Second
				}
				remaining := int(time.Until(retryDeadline).Seconds())
				log.Printf("[流式重试] 账号 %s 错误 HTTP %d，换账号重试 #%d/%d (剩余 %ds)",
					acc.Email, streamStatus, attempt, maxAttempts, remaining)
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					goto streamDone
				}
				newAcc := s.accountMgr.GetNextAccountWait(r.Context(), sessionKey, model)
				if newAcc != nil && newAcc.ID != acc.ID {
					acc.ReleaseSlot()
					acc = newAcc
				}
			}
		}

	streamDone:

		if streamErr != nil || streamResult == nil {
			acc.SetRequestDone("error", time.Since(startTime))
			acc.ReleaseSlot()
			openaiSlotReleased = true
			s.recordUsage(legacy.UsageRecord{
				Timestamp:    time.Now(),
				APIKey:       apiKey,
				Model:        model,
				Protocol:     "openai",
				AccountEmail: acc.Email,
				InputTokens:  inputTokens,
				Success:      false,
				DurationMs:   time.Since(startTime).Milliseconds(),
			})
			return
		}
		// Record streaming usage
		streamContent := strings.Join(streamResult.Content, "")
		// 记录响应：文本 + tool_use 都记录
		var streamResponse interface{}
		if len(streamResult.ToolUses) > 0 {
			var toolUses []interface{}
			for _, raw := range streamResult.ToolUses {
				var tu interface{}
				json.Unmarshal(raw, &tu)
				toolUses = append(toolUses, tu)
			}
			if streamContent != "" {
				streamResponse = map[string]interface{}{"text": streamContent, "tool_uses": toolUses}
			} else {
				streamResponse = map[string]interface{}{"tool_uses": toolUses}
			}
		} else {
			streamResponse = streamContent
		}
		// output token = 文本 + tool_use（都算输出）
		_, streamOutTokens := legacy.EstimateTokens(nil, streamContent)
		for _, raw := range streamResult.ToolUses {
			streamOutTokens += estimateTokens(string(raw))
		}
		streamDurationMs := time.Since(startTime).Milliseconds()
		s.recordUsage(legacy.UsageRecord{
			Timestamp:    time.Now(),
			APIKey:       apiKey,
			Model:        model,
			Protocol:     "openai",
			AccountEmail: acc.Email,
			InputTokens:  inputTokens,
			OutputTokens: streamOutTokens,
			TotalTokens:  inputTokens + streamOutTokens,
			CreditsUsed:  streamResult.CreditsUsed,
			Success:      true,
			DurationMs:   streamDurationMs,
		})
		go SaveRequestLog(&RequestLog{
			ID: uuid.New().String()[:8], Timestamp: time.Now().Format(time.RFC3339),
			Protocol: "openai", Model: model, APIKey: maskAPIKey(apiKey), AccountEmail: acc.Email,
			SessionID: sessionID, SessionKey: sessionKey, ConversationID: kiro.BuildConversationID(model, history, userContent),
			IsNewConv: openaiCache.ReadTokens == 0 && openaiCache.CreationTokens == 0,
			Request: body, Response: streamResponse,
			InputTokens: inputTokens, OutputTokens: streamOutTokens,
			CacheRead: openaiCache.ReadTokens, CacheCreation: openaiCache.CreationTokens,
			DurationMs: streamDurationMs, Success: true,
		})
		acc.RecordSuccess()
		acc.SetRequestDone("success", time.Since(startTime))
		acc.ReleaseSlot()
		openaiSlotReleased = true
		if streamResult.CreditsUsed > 0 {
			acc.RecordCredits(streamResult.CreditsUsed, streamResult.ContextUsagePercent)
		}
		s.sessionTracker.UpdateContext(sessionKey, streamResult.ContextUsagePercent)
		s.sessionTracker.UpdateCachePerMessage(openaiMsgHashes, openaiHashes)
		return
	}

	openaiSlotReleased = true // sendWithRetry 内部管理 slot
	result, statusCode, actualAcc, err := s.sendWithRetry(acc, model, userContent, history, kiroTools, images, toolResults)
	if actualAcc != nil {
		acc = actualAcc
	}
	if err != nil {
		// Record failed usage
		s.recordUsage(legacy.UsageRecord{
			Timestamp:    time.Now(),
			APIKey:       apiKey,
			Model:        model,
			Protocol:     "openai",
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
				"message": err.Error(),
				"type":    string(kiroErr.Type),
			},
		})
		return
	}

	content := strings.Join(result.Content, "")
	var openaiResponse interface{}
	if len(result.ToolUses) > 0 {
		var toolUses []interface{}
		for _, raw := range result.ToolUses {
			var tu interface{}
			json.Unmarshal(raw, &tu)
			toolUses = append(toolUses, tu)
		}
		if content != "" {
			openaiResponse = map[string]interface{}{"text": content, "tool_uses": toolUses}
		} else {
			openaiResponse = map[string]interface{}{"tool_uses": toolUses}
		}
	} else {
		openaiResponse = content
	}
	_, outputTokens := legacy.EstimateTokens(nil, content)
	for _, raw := range result.ToolUses {
		outputTokens += estimateTokens(string(raw))
	}

	// Record successful usage
	durationMs := time.Since(startTime).Milliseconds()
	s.recordUsage(legacy.UsageRecord{
		Timestamp:    time.Now(),
		APIKey:       apiKey,
		Model:        model,
		Protocol:     "openai",
		AccountEmail: acc.Email,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Success:      true,
		DurationMs:   durationMs,
	})
	go SaveRequestLog(&RequestLog{
		ID: uuid.New().String()[:8], Timestamp: time.Now().Format(time.RFC3339),
		Protocol: "openai", Model: model, APIKey: maskAPIKey(apiKey), AccountEmail: acc.Email,
		SessionID: sessionID, SessionKey: sessionKey, ConversationID: kiro.BuildConversationID(model, history, userContent),
		IsNewConv: openaiCache.ReadTokens == 0 && openaiCache.CreationTokens == 0,
		Request: body, Response: openaiResponse,
		InputTokens: inputTokens, OutputTokens: outputTokens,
		CacheRead: openaiCache.ReadTokens, CacheCreation: openaiCache.CreationTokens,
		DurationMs: durationMs, Success: true,
	})

	// 更新会话上下文使用率
	s.sessionTracker.UpdateContext(sessionKey, result.ContextUsagePercent)

	// Prompt caching: 逐条 message hash 匹配
	openaiCacheNonStream := CacheResult{}
	if s.EnableCache {
		openaiCacheNonStream = s.sessionTracker.GetCachedTokensPerMessage(openaiMsgHashes, openaiHashes)
	}
	s.sessionTracker.UpdateCachePerMessage(openaiMsgHashes, openaiHashes)

	resp := map[string]interface{}{
		"id":      "chatcmpl-" + uuid.New().String()[:8],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": mapStopReason(result.StopReason, "openai"),
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
			"prompt_tokens_details": map[string]interface{}{
				"cached_tokens": openaiCacheNonStream.ReadTokens,
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

	if len(result.ToolUses) > 0 {
		var toolCalls []map[string]interface{}
		for _, raw := range result.ToolUses {
			var tu map[string]interface{}
			json.Unmarshal(raw, &tu)
			args, _ := json.Marshal(tu["input"])
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   tu["id"],
				"type": "function",
				"function": map[string]interface{}{
					"name":      tu["name"],
					"arguments": string(args),
				},
			})
		}
		resp["choices"].([]map[string]interface{})[0]["message"].(map[string]interface{})["tool_calls"] = toolCalls
		resp["choices"].([]map[string]interface{})[0]["finish_reason"] = "tool_calls"
	}

	httputil.WriteJSON(w, 200, resp)
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		httputil.WriteJSON(w, 405, map[string]interface{}{"error": map[string]interface{}{"type": "invalid_request_error", "message": "Method not allowed"}})
		return
	}

	startTime := time.Now()
	apiKey := r.Header.Get("X-Resolved-Key")

	// Read request body (limit to 10MB)
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": map[string]interface{}{"type": "invalid_request_error", "message": "Failed to read body"}})
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": map[string]interface{}{"type": "invalid_request_error", "message": "Invalid JSON"}})
		return
	}

	messages, _ := body["messages"].([]interface{})
	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	tools, _ := body["tools"].([]interface{})

	if len(messages) == 0 {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": map[string]interface{}{"type": "invalid_request_error", "message": "messages is required"}})
		return
	}

	// 自动回复拦截
	if reply, matched := s.checkAutoReply(messages); matched {
		if stream {
			SendAutoReplySSE(w, reply, model)
		} else {
			SendAutoReplyJSON(w, reply, model)
		}
		return
	}

	originalModel := model
	model = kiro.MapModelName(model)
	if model != originalModel {
		log.Printf("[Anthropic] 模型映射: %s → %s", originalModel, model)
	}

	// 在 system 插入前生成 sessionID
	sessionID := convert.GenerateSessionID(messages)

	// 在 system 插入前计算 hash 和估算 token
	anthropicHashes := ComputeContentHash(body)
	anthropicMsgHashes := ComputeMessageHashes(messages)
	anthropicInputTokens := anthropicHashes.BaseTokens
	for _, mh := range anthropicMsgHashes {
		anthropicInputTokens += mh.Tokens
	}

	// 在 system 插入前提取 sessionKey
	sessionKey := extractSessionKey(r, body, messages)

	// Prepend system message
	if system, ok := body["system"]; ok {
		var systemText string
		switch sv := system.(type) {
		case string:
			systemText = sv
		case []interface{}:
			for _, block := range sv {
				if b, ok := block.(map[string]interface{}); ok {
					if t, ok := b["text"].(string); ok {
						systemText += t + "\n"
					}
				}
			}
		}
		if systemText != "" {
			messages = append([]interface{}{
				map[string]interface{}{"role": "user", "content": "[System] " + systemText},
				map[string]interface{}{"role": "assistant", "content": "I understand."},
			}, messages...)
		}
	}

	acc := s.accountMgr.GetNextAccountWait(r.Context(), sessionID, model)
	if acc == nil {
		httputil.WriteJSON(w, 503, map[string]interface{}{"error": map[string]interface{}{"type": "overloaded_error", "message": "No available accounts"}})
		return
	}

	slotReleased := false
	defer func() {
		if !slotReleased {
			log.Printf("[Anthropic] ⚠️ 安全网释放 slot: %s (active=%d)", acc.Email, atomic.LoadInt64(&acc.ActiveRequests))
			acc.ReleaseSlot()
		}
	}()

	if originalModel != model {
		log.Printf("[Anthropic] %s (原始: %s) → %s | msgs=%d", model, originalModel, acc.Email, len(messages))
	} else {
		log.Printf("[Anthropic] %s → %s | msgs=%d", model, acc.Email, len(messages))
	}

	userContent, history, kiroTools, images, toolResults := convert.ConvertAnthropicMessagesToKiro(messages, tools)

	// 上下文续期
	ctxUsage := s.sessionTracker.GetContextUsage(sessionKey)
	if ctxUsage >= 85.0 && len(history) > 5 {
		summaryHistory := history
		if len(history) > 6 {
			summaryHistory = history[:len(history)-3]
		}
		summary := s.historyMgr.SummarizeForRenewal(summaryHistory)
		recentHistory := history
		if len(history) > 3 {
			recentHistory = history[len(history)-3:]
		}
		var newHistory []interface{}
		if summary != "" {
			newHistory = append(newHistory, map[string]interface{}{
				"userInputMessage": map[string]interface{}{"content": "[对话摘要]\n" + summary, "modelId": model, "origin": "AI_EDITOR"},
			})
			newHistory = append(newHistory, map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{"content": "我已了解之前的对话内容，请继续。"},
			})
		}
		newHistory = append(newHistory, recentHistory...)
		log.Printf("[Anthropic] 🔄 上下文 %.1f%% → 开新会话: %d 条历史 → 摘要(%d字) + %d 条最近历史",
			ctxUsage, len(history), len(summary), len(recentHistory))
		history = newHistory
		s.sessionTracker.RenewSession(sessionKey)
		s.sessionTracker.SetSummary(sessionKey, summary)
	} else if ctxUsage > 70.0 && s.historyMgr.ShouldCompress(history) {
		beforeLen := len(history)
		history = s.historyMgr.CompressHistory(history, model)
		log.Printf("[Anthropic] ⚠️ 上下文 %.1f%% 超过阈值，压缩历史: %d → %d 条", ctxUsage, beforeLen, len(history))
	}

	inputTokens := anthropicInputTokens

	// Prompt caching
	anthropicCache := CacheResult{}
	if s.EnableCache {
		anthropicCache = s.sessionTracker.GetCachedTokensPerMessage(anthropicMsgHashes, anthropicHashes)
	}

	if stream {
		var streamResult *kiro.KiroResponse
		var streamErr error
		retryCfg := legacy.GlobalRateLimiter.GetConfig()
		maxAttempts := retryCfg.RetryMaxAttempts
		if maxAttempts <= 0 { maxAttempts = 100 }
		timeoutSecs := retryCfg.RetryTimeoutSeconds
		if timeoutSecs <= 0 { timeoutSecs = 60 }
		retryDeadline := time.Now().Add(time.Duration(timeoutSecs) * time.Second)
		attempt := 0

		for attempt < maxAttempts && time.Now().Before(retryDeadline) {
			select {
			case <-r.Context().Done():
				goto anthropicStreamDone
			default:
			}
			profileArn := ""
			if acc.Credentials != nil { profileArn = acc.Credentials.ProfileArn }
			var streamStatus int
			streamResult, streamStatus, streamErr = StreamAnthropicFromKiro(
				w, r, s.kiroClient, acc.GetToken(), acc.GetMachineID(),
				model, userContent, history, kiroTools, images, toolResults,
				profileArn, model, inputTokens, anthropicCache, s.proxyMgr.GetTransportForAccount(acc.ID),
			)
			if streamErr == nil { acc.SetRequestStatus("streaming"); break }
			isRetryable := streamStatus == 429 || streamStatus >= 500 || streamStatus == 0 || strings.Contains(streamErr.Error(), "high traffic") || strings.Contains(streamErr.Error(), "EOF")
			if !isRetryable { break }
			attempt++
			if streamStatus == 429 {
				delay := time.Duration(retryCfg.Retry429DelaySeconds * float64(time.Second))
				if delay > 0 {
					select { case <-time.After(delay): case <-r.Context().Done(): goto anthropicStreamDone }
				}
			} else {
				if streamStatus != 0 { acc.RecordError(streamStatus, streamErr.Error()) }
				delay := time.Duration(retryCfg.RetryErrorDelaySeconds * float64(time.Second))
				if delay <= 0 { delay = 1 * time.Second }
				select { case <-time.After(delay): case <-r.Context().Done(): goto anthropicStreamDone }
				newAcc := s.accountMgr.GetNextAccountWait(r.Context(), sessionKey, model)
				if newAcc != nil && newAcc.ID != acc.ID { acc.ReleaseSlot(); acc = newAcc }
			}
		}

	anthropicStreamDone:
		defer func() { acc.ReleaseSlot(); slotReleased = true }()

		if streamErr != nil || streamResult == nil {
			acc.SetRequestDone("error", time.Since(startTime))
			s.recordUsage(legacy.UsageRecord{Timestamp: time.Now(), APIKey: apiKey, Model: model, Protocol: "anthropic", AccountEmail: acc.Email, InputTokens: inputTokens, Success: false, DurationMs: time.Since(startTime).Milliseconds()})
			return
		}
		streamContent := strings.Join(streamResult.Content, "")
		var aStreamResponse interface{}
		if len(streamResult.ToolUses) > 0 {
			var tus []interface{}
			for _, raw := range streamResult.ToolUses { var tu interface{}; json.Unmarshal(raw, &tu); tus = append(tus, tu) }
			if streamContent != "" {
				aStreamResponse = map[string]interface{}{"text": streamContent, "tool_uses": tus}
			} else {
				aStreamResponse = map[string]interface{}{"tool_uses": tus}
			}
		} else {
			aStreamResponse = streamContent
		}
		_, streamOutTokens := legacy.EstimateTokens(nil, streamContent)
		for _, raw := range streamResult.ToolUses { streamOutTokens += estimateTokens(string(raw)) }
		aStreamDurationMs := time.Since(startTime).Milliseconds()
		s.recordUsage(legacy.UsageRecord{Timestamp: time.Now(), APIKey: apiKey, Model: model, Protocol: "anthropic", AccountEmail: acc.Email, InputTokens: inputTokens, OutputTokens: streamOutTokens, TotalTokens: inputTokens + streamOutTokens, CreditsUsed: streamResult.CreditsUsed, Success: true, DurationMs: aStreamDurationMs})
		go SaveRequestLog(&RequestLog{
			ID: uuid.New().String()[:8], Timestamp: time.Now().Format(time.RFC3339),
			Protocol: "anthropic", Model: model, APIKey: maskAPIKey(apiKey), AccountEmail: acc.Email,
			SessionID: sessionID, SessionKey: sessionKey, ConversationID: kiro.BuildConversationID(model, history, userContent),
			IsNewConv: anthropicCache.ReadTokens == 0 && anthropicCache.CreationTokens == 0,
			Request: body, Response: aStreamResponse,
			InputTokens: inputTokens, OutputTokens: streamOutTokens,
			CacheRead: anthropicCache.ReadTokens, CacheCreation: anthropicCache.CreationTokens,
			DurationMs: aStreamDurationMs, Success: true,
		})
		acc.RecordSuccess()
		acc.SetRequestDone("success", time.Since(startTime))
		if streamResult.CreditsUsed > 0 { acc.RecordCredits(streamResult.CreditsUsed, streamResult.ContextUsagePercent) }
		s.sessionTracker.UpdateContext(sessionKey, streamResult.ContextUsagePercent)
		s.sessionTracker.UpdateCachePerMessage(anthropicMsgHashes, anthropicHashes)
		return
	}

	slotReleased = true
	result, statusCode, actualAcc, err := s.sendWithRetry(acc, model, userContent, history, kiroTools, images, toolResults)
	if actualAcc != nil { acc = actualAcc }
	if err != nil {
		s.recordUsage(legacy.UsageRecord{Timestamp: time.Now(), APIKey: apiKey, Model: model, Protocol: "anthropic", AccountEmail: acc.Email, InputTokens: inputTokens, Success: false, DurationMs: time.Since(startTime).Milliseconds()})
		kiroErr := kiro.ClassifyError(statusCode, err.Error())
		errType := kiro.GetAnthropicErrorType(kiroErr.Type)
		httputil.WriteJSON(w, statusCode, map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": errType, "message": err.Error()}})
		return
	}

	aContent := strings.Join(result.Content, "")
	var anthropicResponse interface{}
	if len(result.ToolUses) > 0 {
		var tus []interface{}
		for _, raw := range result.ToolUses { var tu interface{}; json.Unmarshal(raw, &tu); tus = append(tus, tu) }
		if aContent != "" {
			anthropicResponse = map[string]interface{}{"text": aContent, "tool_uses": tus}
		} else {
			anthropicResponse = map[string]interface{}{"tool_uses": tus}
		}
	} else {
		anthropicResponse = aContent
	}
	_, aOutputTokens := legacy.EstimateTokens(nil, aContent)
	for _, raw := range result.ToolUses { aOutputTokens += estimateTokens(string(raw)) }
	aDurationMs := time.Since(startTime).Milliseconds()
	s.recordUsage(legacy.UsageRecord{Timestamp: time.Now(), APIKey: apiKey, Model: model, Protocol: "anthropic", AccountEmail: acc.Email, InputTokens: inputTokens, OutputTokens: aOutputTokens, TotalTokens: inputTokens + aOutputTokens, Success: true, DurationMs: aDurationMs})
	go SaveRequestLog(&RequestLog{
		ID: uuid.New().String()[:8], Timestamp: time.Now().Format(time.RFC3339),
		Protocol: "anthropic", Model: model, APIKey: maskAPIKey(apiKey), AccountEmail: acc.Email,
		SessionID: sessionID, SessionKey: sessionKey, ConversationID: kiro.BuildConversationID(model, history, userContent),
		IsNewConv: anthropicCache.ReadTokens == 0 && anthropicCache.CreationTokens == 0,
		Request: body, Response: anthropicResponse,
		InputTokens: inputTokens, OutputTokens: aOutputTokens,
		CacheRead: anthropicCache.ReadTokens, CacheCreation: anthropicCache.CreationTokens,
		DurationMs: aDurationMs, Success: true,
	})

	s.sessionTracker.UpdateContext(sessionKey, result.ContextUsagePercent)
	s.sessionTracker.UpdateCachePerMessage(anthropicMsgHashes, anthropicHashes)

	var contentBlocks []interface{}
	if aContent != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": aContent})
	}
	for _, raw := range result.ToolUses {
		var tu map[string]interface{}
		json.Unmarshal(raw, &tu)
		contentBlocks = append(contentBlocks, map[string]interface{}{"type": "tool_use", "id": tu["id"], "name": tu["name"], "input": tu["input"]})
	}
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{"type": "text", "text": ""})
	}

	stopReason := "end_turn"
	if len(result.ToolUses) > 0 { stopReason = "tool_use" }

	anthropicFinalInput := inputTokens - anthropicCache.ReadTokens - anthropicCache.CreationTokens
	if anthropicFinalInput < 0 { anthropicFinalInput = 0 }

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"id": "msg_" + uuid.New().String()[:12], "type": "message", "role": "assistant", "model": model,
		"content": contentBlocks, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens": anthropicFinalInput, "output_tokens": aOutputTokens,
			"cache_creation_input_tokens": anthropicCache.CreationTokens,
			"cache_read_input_tokens":     anthropicCache.ReadTokens,
		},
	})
}

func (s *Server) sendWithRetry(acc *Account, model, userContent string, history, kiroTools, images, toolResults []interface{}) (*kiro.KiroResponse, int, *Account, error) {
	const maxRounds = 10
	const retryInterval = 1 * time.Second
	contentTruncated := false // track if we already truncated for content_too_long

	// Release the initial slot acquired by GetNextAccount — we manage slots ourselves in the retry loop
	acc.ReleaseSlot()

	allAccounts := s.accountMgr.GetAllAccounts()

	var lastErr error
	var lastStatusCode int

	for round := 0; round < maxRounds; round++ {
		// On first round, use the provided account; on subsequent rounds, try all accounts
		var accountsToTry []*Account
		if round == 0 {
			// Start with the assigned account (only if it supports this model)
			if acc.SupportsModel(model) {
				accountsToTry = append(accountsToTry, acc)
			}
			for _, a := range allAccounts {
				if a.Email != acc.Email && a.IsAvailable() && a.SupportsModel(model) {
					accountsToTry = append(accountsToTry, a)
				}
			}
		} else {
			// Rotate: try all available accounts that support this model
			for _, a := range allAccounts {
				if a.IsAvailable() && a.SupportsModel(model) {
					accountsToTry = append(accountsToTry, a)
				}
			}
			// If no matching accounts, use all enabled ones that support this model
			if len(accountsToTry) == 0 {
				for _, a := range allAccounts {
					if a.Enabled && a.SupportsModel(model) {
						accountsToTry = append(accountsToTry, a)
					}
				}
			}
		}

		for i := 0; i < len(accountsToTry); i++ {
			tryAcc := accountsToTry[i]

			// Rate limit check: skip this account if rate limited
			canReq, waitSec, reason := legacy.GlobalRateLimiter.CanRequest(tryAcc.ID)
			if !canReq {
				log.Printf("[限流] ⏳ %s 被限流: %s (等待%.1fs)", tryAcc.Email, reason, waitSec)
				continue
			}

			// Acquire slot for this attempt
			if !tryAcc.AcquireSlot() {
				// Slot full, skip
				continue
			}

			token := tryAcc.GetToken()
			machineID := tryAcc.GetMachineID()

			// Use account-bound proxy, or fallback to any available
			proxyTransport := s.proxyMgr.GetTransportForAccount(tryAcc.ID)
			if proxyTransport == nil {
				proxyTransport = s.proxyMgr.GetTransport("api")
			}

			// Record request for rate limiting BEFORE sending
			legacy.GlobalRateLimiter.RecordRequest(tryAcc.ID)

			// 获取当前账号的 profileArn
			profileArn := ""
			if tryAcc.Credentials != nil {
				profileArn = tryAcc.Credentials.ProfileArn
			}

			result, statusCode, err := s.kiroClient.SendRequest(token, machineID, model, userContent, history, kiroTools, images, toolResults, profileArn, proxyTransport)

			// If proxy caused a network error, retry without proxy
			if err != nil && statusCode == 0 && proxyTransport != nil {
				log.Printf("[请求] ⚠️ 代理连接失败，回退直连重试: %s", kiro.TruncStr(err.Error(), 100))
				result, statusCode, err = s.kiroClient.SendRequest(token, machineID, model, userContent, history, kiroTools, images, toolResults, profileArn)
			}

			// Success!
			if err == nil {
				tryAcc.ReleaseSlot()
				tryAcc.RecordSuccess()
				tryAcc.RecordCredits(result.CreditsUsed, result.ContextUsagePercent)
				creditsLog := ""
				if result.CreditsUsed > 0 {
					creditsLog = fmt.Sprintf(" | credits=%.4f ctx=%.1f%%", result.CreditsUsed, result.ContextUsagePercent)
				}
				if tryAcc.Email != acc.Email {
					log.Printf("[请求] ✅ %s → %s (重试成功)%s", acc.Email, tryAcc.Email, creditsLog)
				} else {
					log.Printf("[请求] ✅ %s%s", tryAcc.Email, creditsLog)
				}
				return result, 200, tryAcc, nil
			}

			// Classify error using unified error handler
			kiroErr := kiro.ClassifyError(statusCode, err.Error())

			// Record error and release slot
			if statusCode != 429 {
				tryAcc.RecordError(statusCode, err.Error())
			}
			tryAcc.ReleaseSlot()

			lastErr = err
			lastStatusCode = statusCode

			// Log the error with classification
			if statusCode == 0 {
				log.Printf("[请求] ❌ %s 网络错误: %s", tryAcc.Email, kiro.TruncStr(err.Error(), 150))
				lastStatusCode = 502
				break
			}
			log.Printf("[请求] ❌ %s [%s] HTTP %d: %s", tryAcc.Email, kiroErr.Type, statusCode, kiro.TruncStr(err.Error(), 150))

			// Content too long: auto-truncate history and retry with SAME account
			if kiroErr.Type == kiro.ErrorContentTooLong && !contentTruncated && len(history) > 4 {
				contentTruncated = true
				beforeLen := len(history)
				// Force aggressive compression
				history = s.historyMgr.CompressHistory(history, model)
				// If CompressHistory didn't reduce enough, hard truncate
				if len(history) > 20 {
					history = history[len(history)-20:]
				}
				log.Printf("[请求] ✂️ 内容过长，自动截断历史: %d → %d 条，使用同一账号重试", beforeLen, len(history))
				// Retry with same account by decrementing i
				i--
				continue
			}

			// Handle based on classified error behavior flags
			if kiroErr.ShouldDisableAccount {
				log.Printf("[重试] ⛔ 账号 %s 错误类型=%s，已禁用，跳过", tryAcc.Email, kiroErr.Type)
				continue
			}

			if kiroErr.ShouldSwitchAccount {
				log.Printf("[请求] ⚠️ %s %s，切换账号 (轮次%d/%d)", tryAcc.Email, kiroErr.UserMessage, round+1, maxRounds)
				continue
			}

			if kiroErr.ShouldRetry {
				log.Printf("[请求] 🔄 %s %s，可重试", tryAcc.Email, kiroErr.UserMessage)
				continue
			}

			// Non-retryable error: return immediately
			if lastStatusCode == 0 {
				lastStatusCode = 502
			}
			return nil, lastStatusCode, nil, err
		}

		// All accounts exhausted in this round, wait before next round
		if round < maxRounds-1 {
			log.Printf("[重试] 🔄 所有账号都被限流/不可用，等待 %v 后进入第 %d/%d 轮", retryInterval, round+2, maxRounds)
			time.Sleep(retryInterval)
		}
	}

	// All rounds exhausted
	if lastErr != nil {
		if lastStatusCode == 0 {
			lastStatusCode = 429
		}
		log.Printf("[重试] ❌ 已用尽全部 %d 轮重试，返回最后错误 (状态码 %d)", maxRounds, lastStatusCode)
		return nil, lastStatusCode, nil, lastErr
	}
	return nil, 502, nil, fmt.Errorf("没有可用的账号")
}

// SetCacheStore 设置缓存后端（Redis 或内存）
func (s *Server) SetCacheStore(store CacheStore) {
	s.sessionTracker.SetCacheStore(store)
}

// secureCompare performs a constant-time string comparison to prevent timing attacks.
func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// CleanupSessions 清理过期的 session tracker 数据
func (s *Server) CleanupSessions() {
	s.sessionTracker.cleanup()
}

// recordUsage 记录用量并更新实时指标
func (s *Server) recordUsage(rec legacy.UsageRecord) {
	s.usageTracker.RecordUsage(rec)
	s.metrics.RecordRequest(rec.InputTokens, rec.OutputTokens)
}

// StartUsageLimitsLoop 启动时查询一次所有账号用量，之后每 10 分钟查询一次
func (s *Server) StartUsageLimitsLoop() {
	go func() {
		// 启动后 10 秒开始第一次查询（等 token 刷新完成）
		time.Sleep(10 * time.Second)
		s.queryAllUsageLimits()

		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.queryAllUsageLimits()
		}
	}()
}

func (s *Server) queryAllUsageLimits() {
	accounts := s.accountMgr.GetAllAccounts()
	queried := 0
	for _, acc := range accounts {
		acc.Mu.Lock()
		if !acc.Enabled || acc.Credentials == nil || acc.Credentials.AccessToken == "" {
			acc.Mu.Unlock()
			continue
		}
		token := acc.Credentials.AccessToken
		machineID := acc.MachineID
		proxyID := acc.ProxyID
		acc.Mu.Unlock()

		var transport http.RoundTripper
		if proxyID != "" {
			transport = s.proxyMgr.GetTransportForAccount(acc.ID)
		}

		var usage *kiro.UsageLimitsResponse
		var err error
		if transport != nil {
			usage, err = s.kiroClient.GetUsageLimits(token, machineID, transport)
		} else {
			usage, err = s.kiroClient.GetUsageLimits(token, machineID)
		}
		if err != nil {
			continue
		}

		acc.Mu.Lock()
		acc.UsageLimits = &kiro.KiroUsageLimits{
			SubscriptionTitle: usage.SubscriptionTitle,
			SubscriptionType:  usage.SubscriptionType,
			UsageLimit:        usage.UsageLimit,
			CurrentUsage:      usage.CurrentUsage,
			FreeTrialStatus:   usage.FreeTrialStatus,
			FreeTrialUsage:    usage.FreeTrialUsage,
			FreeTrialLimit:    usage.FreeTrialLimit,
			FreeTrialExpiry:   usage.FreeTrialExpiry,
			DaysUntilReset:    usage.DaysUntilReset,
			QueriedAt:         time.Now(),
		}
		acc.Mu.Unlock()
		queried++

		if s.DB != nil {
			SaveAccountToDB(s.DB, acc)
		}
		time.Sleep(500 * time.Millisecond) // 避免请求太快
	}
	if queried > 0 {
		log.Printf("[UsageLimits] 自动查询了 %d 个账号的用量", queried)
	}
}

// checkAutoReply 从 messages 提取最后一条用户消息，检查是否匹配自动回复规则
func (s *Server) checkAutoReply(messages []interface{}) (string, bool) {
	if s.AutoReply == nil {
		return "", false
	}
	// 从后往前找最后一条 user 消息
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		// 提取文本内容
		content := ""
		switch c := msg["content"].(type) {
		case string:
			content = c
		case []interface{}:
			// Anthropic 格式: [{"type":"text","text":"..."}]
			for _, block := range c {
				if bm, ok := block.(map[string]interface{}); ok {
					if t, ok := bm["text"].(string); ok {
						content += t
					}
				}
			}
		}
		if content == "" {
			continue
		}
		// 只检查纯文本部分（去掉 XML 标签等）
		cleanContent := content
		if idx := strings.Index(cleanContent, "<"); idx > 0 {
			cleanContent = cleanContent[:idx]
		}
		cleanContent = strings.TrimSpace(cleanContent)
		if cleanContent == "" {
			continue
		}
		if reply, ok := s.AutoReply.Match(cleanContent); ok {
			log.Printf("[AutoReply] ✅ 匹配: 关键词命中, 内容=%s", kiro.TruncStr(cleanContent, 50))
			return reply, true
		}
		// 没匹配到，记录一下方便调试
		log.Printf("[AutoReply] 未匹配: %s", kiro.TruncStr(cleanContent, 80))
		return "", false
	}
	return "", false
}
