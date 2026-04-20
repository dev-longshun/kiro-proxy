package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro-proxy/internal/httputil"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
)

// handleCacheToggle 开关 prompt caching
func (s *Server) handleCacheToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		s.EnableCache = !s.EnableCache
		log.Printf("[Cache] Prompt caching %s", map[bool]string{true: "已开启", false: "已关闭"}[s.EnableCache])
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"enabled": s.EnableCache,
	})
}

// handleMetrics returns real-time performance metrics
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// 计算所有账号的总活跃请求数 + 积分汇总
	var totalActive int64
	var totalCurrentUsage, totalUsageLimit float64
	queriedCount := 0
	totalCount := 0
	for _, acc := range s.accountMgr.GetAllAccounts() {
		totalActive += atomic.LoadInt64(&acc.ActiveRequests)
		acc.Mu.Lock()
		if acc.UsageLimits != nil {
			totalCurrentUsage += acc.UsageLimits.CurrentUsage
			totalUsageLimit += acc.UsageLimits.UsageLimit
			queriedCount++
		}
		acc.Mu.Unlock()
		totalCount++
	}
	totalRemaining := totalUsageLimit - totalCurrentUsage
	if totalRemaining < 0 {
		totalRemaining = 0
	}
	queued := atomic.LoadInt64(&s.accountMgr.QueuedRequests)
	metrics := s.metrics.GetMetrics(totalActive, queued)
	// 附加积分汇总
	metrics["credits_summary"] = map[string]interface{}{
		"total_credits_used":      totalCurrentUsage,
		"total_credits_limit":     totalUsageLimit,
		"total_credits_remaining": totalRemaining,
		"account_count":           totalCount,
		"queried_count":           queriedCount,
	}
	httputil.WriteJSON(w, 200, metrics)
}

// handleStatus returns server status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	accountStats := s.accountMgr.GetAccountStats()

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"status":           "running",
		"version":          "2.0.0-go",
		"total_accounts":   accountStats["total"],
		"active_accounts":  accountStats["active"],
		"suspended_accounts": accountStats["suspended"],
		"cooldown_accounts":  accountStats["cooldown"],
		"unhealthy_accounts": accountStats["unhealthy"],
		"total_proxies":    len(s.proxyMgr.GetAllProxies()),
		"usage_summary":    s.usageTracker.GetTotalSummary(),
	})
}

// handleAccounts returns account list or adds an account
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		s.getAccounts(w, r)
	case "POST":
		s.addAccount(w, r)
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

func (s *Server) getAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := s.accountMgr.GetAllAccounts()
	usageByAccount := s.usageTracker.GetSummaryByAccount()
	result := make([]map[string]interface{}, 0, len(accounts))
	for _, acc := range accounts {
		info := map[string]interface{}{
			"id":               acc.ID,
			"email":            acc.Email,
			"nickname":         acc.Nickname,
			"enabled":          acc.Enabled,
			"machine_id":       acc.MachineID,
			"request_count":    atomic.LoadInt64(&acc.RequestCount),
			"error_count":      atomic.LoadInt64(&acc.ErrorCount),
			"active_requests":  atomic.LoadInt64(&acc.ActiveRequests),
			"max_concurrent":   acc.GetMaxConcurrent(),
			"queued":           s.accountMgr.GetAccountQueued(acc.ID),
			"supported_models": acc.SupportedModels,
			"last_used":        acc.LastUsedAt,
			"status":           string(acc.Status),
			"has_token":        acc.GetToken() != "",
			"consecutive_errs": acc.ConsecutiveErrs,
			"proxy_id":         acc.ProxyID,
			// Real-time status fields
			"total_429":              atomic.LoadInt64(&acc.TotalRequests429),
			"total_success":          atomic.LoadInt64(&acc.TotalRequestsOK),
			"total_errors":           atomic.LoadInt64(&acc.TotalRequestsErr),
			"last_request_status":    acc.LastRequestStatus,
			"last_request_time":      acc.LastRequestTime,
			"last_request_duration_ms": acc.LastRequestDuration.Milliseconds(),
		}
		// 脱敏显示 token
		if acc.Credentials != nil {
			token := acc.Credentials.AccessToken
			if len(token) > 20 {
				info["access_token_preview"] = token[:10] + "..." + token[len(token)-6:]
			} else if token != "" {
				info["access_token_preview"] = "***"
			}
			info["has_refresh_token"] = acc.Credentials.RefreshToken != ""
			info["auth_method"] = acc.Credentials.GetAuthMethod()
		}
		// Add proxy info if bound
		if acc.ProxyID != "" {
			if p := s.proxyMgr.GetProxy(acc.ProxyID); p != nil {
				info["proxy_name"] = p.Name
				info["proxy_url"] = legacy.MaskProxyURL(p.URL)
			}
		}
		if acc.Status == kiro.StatusSuspended {
			info["suspended_at"] = acc.SuspendedAt
			info["suspended_reason"] = acc.SuspendedReason
		}
		if acc.Status == kiro.StatusCooldown {
			info["cooldown_until"] = acc.CooldownUntil
		}
		if acc.LastErrorCode > 0 {
			info["last_error_code"] = acc.LastErrorCode
			info["last_error_msg"] = kiro.TruncStr(acc.LastErrorMessage, 100)
		}
		// Attach usage stats
		if usage, ok := usageByAccount[acc.Email]; ok {
			info["usage"] = usage
		}
		// Kiro credits
		info["credits_used"] = acc.CreditsUsed
		info["last_credits"] = acc.LastCreditsUsed
		info["context_usage_pct"] = acc.ContextUsagePercent
		if acc.UsageLimits != nil {
			info["kiro_usage"] = acc.UsageLimits
		}
		result = append(result, info)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"accounts": result,
		"stats":    s.accountMgr.GetAccountStats(),
	})
}

// handleAccountsStatus returns lightweight real-time status for all accounts (fast polling endpoint)
func (s *Server) handleAccountsStatus(w http.ResponseWriter, r *http.Request) {
	accounts := s.accountMgr.GetAllAccounts()
	now := time.Now()
	result := make([]map[string]interface{}, 0, len(accounts))
	for _, acc := range accounts {
		acc.Mu.Lock()
		status := string(acc.Status)
		enabled := acc.Enabled
		lastReqStatus := acc.LastRequestStatus
		lastReqTime := acc.LastRequestTime
		lastReqDur := acc.LastRequestDuration
		lastErrCode := acc.LastErrorCode
		lastErrMsg := acc.LastErrorMessage
		cooldownUntil := acc.CooldownUntil
		rateLimitedUntil := acc.RateLimitedUntil
		recent429 := acc.Recent429Count
		consecutiveErrs := acc.ConsecutiveErrs
		creditsUsed := acc.CreditsUsed
		var usageLimitVal, currentUsageVal float64
		if acc.UsageLimits != nil {
			usageLimitVal = acc.UsageLimits.UsageLimit
			currentUsageVal = acc.UsageLimits.CurrentUsage
		}
		acc.Mu.Unlock()

		active := atomic.LoadInt64(&acc.ActiveRequests)
		maxC := acc.GetMaxConcurrent()

		// Compute effective display status
		displayStatus := status
		if !enabled {
			displayStatus = "disabled"
		} else if active > 0 && lastReqStatus == "streaming" {
			displayStatus = "streaming"
		} else if !rateLimitedUntil.IsZero() && now.Before(rateLimitedUntil) {
			displayStatus = "429-cooldown"
		} else if status == "active" && active == 0 && (lastReqTime.IsZero() || now.Sub(lastReqTime) > 30*time.Second) {
			displayStatus = "idle"
		}

		entry := map[string]interface{}{
			"id":                       acc.ID,
			"active_requests":          active,
			"max_concurrent":           maxC,
			"queued":                   s.accountMgr.GetAccountQueued(acc.ID),
			"status":                   status,
			"display_status":           displayStatus,
			"last_request_status":      lastReqStatus,
			"last_request_duration_ms": lastReqDur.Milliseconds(),
			"total_429":               atomic.LoadInt64(&acc.TotalRequests429),
			"total_success":            atomic.LoadInt64(&acc.TotalRequestsOK),
			"total_errors":             atomic.LoadInt64(&acc.TotalRequestsErr),
			"request_count":            atomic.LoadInt64(&acc.RequestCount),
			"error_count":              atomic.LoadInt64(&acc.ErrorCount),
			"recent_429":              recent429,
			"consecutive_errs":         consecutiveErrs,
			"credits_used":             creditsUsed,
			"usage_limit":              usageLimitVal,
			"current_usage":            currentUsageVal,
		}
		if !lastReqTime.IsZero() {
			entry["last_request_ago_ms"] = now.Sub(lastReqTime).Milliseconds()
			entry["last_request_time"] = lastReqTime
		}
		if lastErrCode > 0 {
			entry["last_error_code"] = lastErrCode
			entry["last_error_msg"] = kiro.TruncStr(lastErrMsg, 80)
		}
		if !cooldownUntil.IsZero() && now.Before(cooldownUntil) {
			entry["cooldown_remaining_ms"] = cooldownUntil.Sub(now).Milliseconds()
		}
		result = append(result, entry)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"accounts":        result,
		"ts":              now.UnixMilli(),
		"total_queued":    atomic.LoadInt64(&s.accountMgr.QueuedRequests),
	})
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Failed to read body"})
		return
	}

	// Check if it's an array — batch import
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []map[string]interface{}
		if err := json.Unmarshal(body, &arr); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON array"})
			return
		}
		imported := 0
		skipped := 0
		for _, item := range arr {
			acc := s.parseAndCreateAccount(item)
			if acc == nil {
				continue
			}
			// 去重：IdC 用 clientId，否则用 accessToken
			if acc.Credentials.ClientID != "" {
				if s.accountMgr.HasClientID(acc.Credentials.ClientID) {
					skipped++
					continue
				}
			} else if s.accountMgr.HasToken(acc.Credentials.AccessToken) {
				skipped++
				continue
			}
			s.accountMgr.AddAccount(acc)
			if s.DB != nil {
				SaveAccountToDB(s.DB, acc)
			}
			imported++
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "imported": imported, "skipped": skipped})
		return
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	acc := s.parseAndCreateAccount(data)
	if acc == nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "需要 accessToken 或 clientId+clientSecret+refreshToken"})
		return
	}

	// 去重：IdC 格式用 clientId 去重，否则用 accessToken
	if acc.Credentials.ClientID != "" {
		if s.accountMgr.HasClientID(acc.Credentials.ClientID) {
			httputil.WriteJSON(w, 409, map[string]interface{}{"error": "该 clientId 的账号已存在"})
			return
		}
	} else if s.accountMgr.HasToken(acc.Credentials.AccessToken) {
		httputil.WriteJSON(w, 409, map[string]interface{}{"error": "该 accessToken 已存在"})
		return
	}

	s.accountMgr.AddAccount(acc)

	// 立即写入数据库
	if s.DB != nil {
		SaveAccountToDB(s.DB, acc)
	}

	// 有 refreshToken 和 clientId 的账号自动刷新 token
	if acc.Credentials != nil && acc.Credentials.RefreshToken != "" && acc.Credentials.ClientID != "" {
		go func() {
			acc.RefreshToken()
			if s.DB != nil {
				SaveAccountToDB(s.DB, acc)
			}
		}()
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"ok":      true,
		"account": map[string]interface{}{"id": acc.ID, "email": acc.Email},
	})
}

// parseAndCreateAccount creates an Account from a JSON map
func (s *Server) parseAndCreateAccount(data map[string]interface{}) *Account {
	email, _ := data["email"].(string)
	nickname, _ := data["nickname"].(string)

	// Support two formats:
	// 1. Simple: { token, refresh_token, email }
	// 2. Full credentials: { clientId, clientSecret, accessToken, refreshToken, email }
	// 3. Nested: { credentials: { clientId, clientSecret, accessToken, refreshToken } }

	creds := &kiro.KiroCredentials{}

	// Check for nested credentials object
	if credsRaw, ok := data["credentials"].(map[string]interface{}); ok {
		creds.ClientID, _ = credsRaw["clientId"].(string)
		creds.ClientSecret, _ = credsRaw["clientSecret"].(string)
		creds.AccessToken, _ = credsRaw["accessToken"].(string)
		creds.RefreshToken, _ = credsRaw["refreshToken"].(string)
		creds.Region, _ = credsRaw["region"].(string)
		creds.AuthMethod, _ = credsRaw["authMethod"].(string)
		creds.ProfileArn, _ = credsRaw["profileArn"].(string)
		creds.Provider, _ = credsRaw["provider"].(string)
	} else {
		// Flat format
		creds.ClientID, _ = data["clientId"].(string)
		creds.ClientSecret, _ = data["clientSecret"].(string)
		creds.ProfileArn, _ = data["profileArn"].(string)
		creds.Region, _ = data["region"].(string)
		creds.AuthMethod, _ = data["authMethod"].(string)
		creds.Provider, _ = data["provider"].(string)

		// accessToken / token
		if at, ok := data["accessToken"].(string); ok && at != "" {
			creds.AccessToken = at
		} else if t, ok := data["token"].(string); ok {
			creds.AccessToken = t
		}
		// refreshToken / refresh_token
		if rt, ok := data["refreshToken"].(string); ok && rt != "" {
			creds.RefreshToken = rt
		} else if rt, ok := data["refresh_token"].(string); ok {
			creds.RefreshToken = rt
		}
	}

	// 允许两种情况：
	// 1. 有 accessToken（直接可用）
	// 2. 有 clientId + clientSecret + refreshToken（IdC 格式，可通过刷新获取 token）
	if creds.AccessToken == "" {
		if creds.ClientID == "" || creds.ClientSecret == "" || creds.RefreshToken == "" {
			return nil
		}
		// IdC 格式：先用占位符，后续刷新会替换
		creds.AccessToken = "pending-refresh-" + creds.ClientID
	}

	// Auto-detect auth method
	if creds.AuthMethod == "" {
		if creds.ClientID != "" && creds.ClientSecret != "" {
			creds.AuthMethod = "idc"
		} else {
			creds.AuthMethod = "social"
		}
	}

	if email == "" {
		email = fmt.Sprintf("manual-%d", time.Now().UnixNano())
	}
	if nickname == "" {
		nickname = email
	}

	acc := &Account{
		ID:          fmt.Sprintf("acc_%d", time.Now().UnixNano()),
		Email:       email,
		Nickname:    nickname,
		MachineID:   kiro.GenerateRandomMachineID(),
		Credentials: creds,
		Enabled:     true,
		Status:      kiro.StatusActive,
	}

	authMethod := creds.GetAuthMethod()
	hasRefresh := creds.RefreshToken != ""
	hasClient := creds.ClientID != ""
	log.Printf("[Account] 添加账号: %s auth=%s hasRefreshToken=%v hasClientId=%v", email, authMethod, hasRefresh, hasClient)

	// 设置状态变化回调（持久化到 DB）
	if s.DB != nil {
		db := s.DB
		acc.OnStatusChange = func(a *Account) {
			SaveAccountToDB(db, a)
		}
	}

	return acc
}

// handleDeleteAccount deletes an account
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "DELETE" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	// Unbind from proxy
	s.proxyMgr.UnbindAccountFromAll(accountID)

	s.accountMgr.RemoveAccount(accountID)

	// 从数据库删除
	if s.DB != nil {
		s.DB.DeleteAccount(accountID)
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
}

// handleToggleAccount toggles account enabled state
func (s *Server) handleToggleAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			acc.Mu.Lock()
			acc.Enabled = !acc.Enabled
			enabled := acc.Enabled
			acc.Mu.Unlock()
			if s.DB != nil {
				s.DB.UpdateAccountField(accountID, "enabled", legacy.BoolToInt(enabled))
			}
			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "enabled": enabled})
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// handleRefreshAll refreshes all expiring tokens
func (s *Server) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	refreshed := 0
	for _, acc := range accounts {
		if acc.Credentials != nil && acc.Credentials.RefreshToken != "" {
			transport := s.proxyMgr.GetTransport("refresh")
			success, msg := acc.RefreshToken(transport)
			if !success && transport != nil {
				log.Printf("[Admin] 代理刷新失败，回退直连: %s", kiro.TruncStr(msg, 100))
				success, msg = acc.RefreshToken()
			}
			if success {
				refreshed++
				// 刷新成功后写 DB
				if s.DB != nil {
					SaveAccountToDB(s.DB, acc)
				}
			} else {
				log.Printf("[Admin] Failed to refresh %s: %s", acc.Email, msg)
			}
		}
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"ok":        true,
		"refreshed": refreshed,
		"total":     len(accounts),
	})
}

// handleTokenStatus returns token status for all accounts
func (s *Server) handleTokenStatus(w http.ResponseWriter, r *http.Request) {
	accounts := s.accountMgr.GetAllAccounts()
	result := make([]map[string]interface{}, 0, len(accounts))
	for _, acc := range accounts {
		result = append(result, map[string]interface{}{
			"id":        acc.ID,
			"email":     acc.Email,
			"has_token": acc.GetToken() != "",
			"enabled":   acc.Enabled,
			"status":    string(acc.Status),
		})
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"accounts": result})
}

// handleExportAccounts exports all accounts
func (s *Server) handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := s.accountMgr.GetAllAccounts()
	exportData := make([]map[string]interface{}, 0, len(accounts))
	for _, acc := range accounts {
		accData := map[string]interface{}{
			"id":       acc.ID,
			"email":    acc.Email,
			"nickname": acc.Nickname,
			"enabled":  acc.Enabled,
		}
		if acc.Credentials != nil {
			accData["access_token"] = acc.Credentials.AccessToken
			accData["refresh_token"] = acc.Credentials.RefreshToken
		}
		exportData = append(exportData, accData)
	}
	httputil.WriteJSON(w, 200, map[string]interface{}{"accounts": exportData})
}

// handleImportAccounts imports accounts from JSON
func (s *Server) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Failed to read body"})
		return
	}

	// 支持三种格式：
	// 1. results5.json 格式: [{clientId, clientSecret, accessToken, refreshToken}, ...]
	// 2. 包装格式: {"accounts": [{...}, ...]}
	// 3. 简单格式: [{access_token, refresh_token, email}, ...]
	trimmed := strings.TrimSpace(string(body))
	var items []map[string]interface{}

	if len(trimmed) > 0 && trimmed[0] == '[' {
		// 直接数组格式
		if err := json.Unmarshal(body, &items); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "JSON 格式错误"})
			return
		}
	} else {
		// {"accounts": [...]} 包装格式
		var wrapper struct {
			Accounts []map[string]interface{} `json:"accounts"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "JSON 格式错误"})
			return
		}
		items = wrapper.Accounts
	}

	imported := 0
	skipped := 0
	for _, item := range items {
		acc := s.parseAndCreateAccount(item)
		if acc == nil {
			continue
		}
		if s.accountMgr.HasToken(acc.Credentials.AccessToken) {
			skipped++
			continue
		}
		s.accountMgr.AddAccount(acc)
		if s.DB != nil {
			SaveAccountToDB(s.DB, acc)
		}
		imported++
	}

	// 导入后自动刷新所有新账号的 token
	if imported > 0 {
		go func() {
			accounts := s.accountMgr.GetAllAccounts()
			refreshed := 0
			for _, acc := range accounts {
				if acc.Credentials != nil && acc.Credentials.RefreshToken != "" && acc.Credentials.ClientID != "" {
					ok, _ := acc.RefreshToken()
					if ok {
						refreshed++
						if s.DB != nil {
							SaveAccountToDB(s.DB, acc)
						}
					}
				}
			}
			if refreshed > 0 {
				log.Printf("[Import] 自动刷新了 %d 个账号的 Token", refreshed)
			}
		}()
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"ok":       true,
		"imported": imported,
		"skipped":  skipped,
	})
}

// handleRefreshAccount refreshes a specific account's token
func (s *Server) handleRefreshAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			transport := s.proxyMgr.GetTransport("refresh")
			success, msg := acc.RefreshToken(transport)
			// If proxy failed, retry without proxy
			if !success && transport != nil {
				log.Printf("[Refresh] 代理失败，回退直连: %s", kiro.TruncStr(msg, 100))
				success, msg = acc.RefreshToken()
			}
			if success {
				if s.DB != nil {
					SaveAccountToDB(s.DB, acc)
				}
				httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
			} else {
				httputil.WriteJSON(w, 500, map[string]interface{}{"error": msg})
			}
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// handleRestoreAccount restores an account from cooldown
func (s *Server) handleRestoreAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			acc.Unsuspend()
			acc.Mu.Lock()
			acc.Enabled = true
			acc.Mu.Unlock()
			if s.DB != nil {
				s.DB.UpdateAccountField(accountID, "enabled", 1)
				s.DB.UpdateAccountField(accountID, "status", "active")
				s.DB.UpdateAccountField(accountID, "suspended_at", "")
				s.DB.UpdateAccountField(accountID, "suspended_reason", "")
				s.DB.UpdateAccountField(accountID, "consecutive_errs", 0)
			}
			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// handleBindProxy binds a proxy to an account
func (s *Server) handleBindProxy(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var body struct {
		ProxyID string `json:"proxy_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProxyID == "" {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "proxy_id is required"})
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			acc.Mu.Lock()
			// Unbind from old proxy first
			if acc.ProxyID != "" && acc.ProxyID != body.ProxyID {
				s.proxyMgr.UnbindAccount(acc.ProxyID, accountID)
			}
			if s.proxyMgr.BindAccount(body.ProxyID, accountID) {
				acc.ProxyID = body.ProxyID
				acc.Mu.Unlock()
				if s.DB != nil {
					s.DB.UpdateAccountField(accountID, "proxy_id", body.ProxyID)
				}
				httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
			} else {
				acc.Mu.Unlock()
				httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Proxy is full or not found"})
			}
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// handleUnbindProxy unbinds proxy from an account
func (s *Server) handleUnbindProxy(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			acc.Mu.Lock()
			if acc.ProxyID != "" {
				s.proxyMgr.UnbindAccount(acc.ProxyID, accountID)
				acc.ProxyID = ""
			}
			acc.Mu.Unlock()
			if s.DB != nil {
				s.DB.UpdateAccountField(accountID, "proxy_id", "")
			}
			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// handleEventLogging handles event logging batch endpoint
func (s *Server) handleEventLogging(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
}

// ==================== Pre-Proxy (Chain Proxy) API ====================

// handlePreProxy GET: get pre_proxy, POST: set pre_proxy
func (s *Server) handlePreProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"pre_proxy": s.proxyMgr.GetPreProxy(),
		})
	case "POST":
		var body struct {
			PreProxy string `json:"pre_proxy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		s.proxyMgr.SetPreProxy(body.PreProxy)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "pre_proxy": body.PreProxy})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// ==================== Proxy Management API ====================

// handleProxies handles GET (list) and POST (create) for proxies
func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		proxies := s.proxyMgr.GetAllProxies()
		result := make([]map[string]interface{}, 0, len(proxies))
		for _, p := range proxies {
			result = append(result, map[string]interface{}{
				"id":              p.ID,
				"name":            p.Name,
				"url":             legacy.MaskProxyURL(p.URL),
				"type":            p.Type,
				"enabled":         p.Enabled,
				"max_accounts":    p.MaxAccounts,
				"bound_accounts":  p.BoundAccounts,
				"bound_count":     len(p.BoundAccounts),
				"available_slots": p.AvailableSlots(),
				"success_count":   p.SuccessCount,
				"error_count":     p.ErrorCount,
				"last_used":       p.LastUsedAt,
				"last_error":      p.LastError,
				"last_latency_ms": p.LastLatency,
				"last_test_ip":    p.LastTestIP,
				"expires_at":      p.ExpiresAt,
				"created_at":      p.CreatedAt,
			})
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{"proxies": result})

	case "POST":
		var body struct {
			Name        string `json:"name"`
			URL         string `json:"url"`
			Type        string `json:"type"`
			MaxAccounts int    `json:"max_accounts"`
			Batch       string `json:"batch"` // multi-line batch import: IP|Port|User|Pass|Expiry
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}

		// Batch import mode
		if body.Batch != "" {
			proxies := legacy.ParseProxyBatch(body.Batch)
			added := 0
			for _, p := range proxies {
				if body.MaxAccounts > 0 {
					p.MaxAccounts = body.MaxAccounts
				}
				s.proxyMgr.AddProxy(p)
				if s.DB != nil {
					s.DB.SaveProxy(p)
				}
				added++
			}
			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "added": added})
			return
		}

		// Single proxy mode — support pipe format in URL field
		if body.URL != "" && !strings.Contains(body.URL, "://") && strings.Contains(body.URL, "|") {
			parsed := legacy.ParseProxyLine(body.URL)
			if parsed == nil {
				httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid proxy format"})
				return
			}
			if body.Name != "" {
				parsed.Name = body.Name
			}
			if body.MaxAccounts > 0 {
				parsed.MaxAccounts = body.MaxAccounts
			}
			if body.Type != "" {
				parsed.Type = body.Type
			}
			s.proxyMgr.AddProxy(parsed)
			if s.DB != nil {
				s.DB.SaveProxy(parsed)
			}
			httputil.WriteJSON(w, 200, map[string]interface{}{
				"ok":    true,
				"proxy": map[string]interface{}{"id": parsed.ID, "name": parsed.Name},
			})
			return
		}

		if body.URL == "" {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "url or batch is required"})
			return
		}
		proxyInfo := &legacy.ProxyInfo{
			Name:        body.Name,
			URL:         body.URL,
			Type:        body.Type,
			MaxAccounts: body.MaxAccounts,
		}
		if proxyInfo.Name == "" {
			proxyInfo.Name = body.URL
		}
		s.proxyMgr.AddProxy(proxyInfo)
		if s.DB != nil {
			s.DB.SaveProxy(proxyInfo)
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"ok":    true,
			"proxy": map[string]interface{}{"id": proxyInfo.ID, "name": proxyInfo.Name},
		})

	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleProxyAction handles individual proxy operations
func (s *Server) handleProxyAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	// Special route: /api/proxies/test-all
	if parts[0] == "test-all" && r.Method == "POST" {
		s.handleTestAllProxies(w, r)
		return
	}

	proxyID := parts[0]

	if len(parts) == 1 {
		if r.Method == "DELETE" {
			unboundAccounts, ok := s.proxyMgr.DeleteProxy(proxyID)
			if ok {
				// 从数据库删除代理
				if s.DB != nil {
					s.DB.DeleteProxy(proxyID)
				}
				for _, accID := range unboundAccounts {
					for _, acc := range s.accountMgr.GetAllAccounts() {
						if acc.ID == accID {
							acc.Mu.Lock()
							acc.ProxyID = ""
							acc.Mu.Unlock()
							// 解绑后写 DB
							if s.DB != nil {
								s.DB.UpdateAccountField(accID, "proxy_id", "")
							}
						}
					}
				}
				httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "unbound": len(unboundAccounts)})
			} else {
				httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Proxy not found"})
			}
		} else if r.Method == "PUT" {
			s.handleUpdateProxy(w, r, proxyID)
		} else {
			http.Error(w, "Method not allowed", 405)
		}
		return
	}

	// 子操作: toggle/test/bind/unbind
	action := parts[1]
	s.handleProxySubAction(w, r, proxyID, action)
}

// handleModelAccounts GET: 获取所有模型-账号映射, POST: 设置某个模型的账号
func (s *Server) handleModelAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		// 返回所有模型-账号映射 + Kiro 支持的模型列表 + 所有账号
		mapping := s.accountMgr.GetAllModelAccounts()
		accounts := s.accountMgr.GetAllAccounts()
		accList := make([]map[string]interface{}, 0, len(accounts))
		for _, acc := range accounts {
			accList = append(accList, map[string]interface{}{
				"id":    acc.ID,
				"email": acc.Email,
			})
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"models": kiro.KiroModels,
			"mapping":  mapping,
			"accounts": accList,
		})
	case "POST":
		var body struct {
			Model      string   `json:"model"`
			AccountIDs []string `json:"account_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		if body.Model == "" {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "model 不能为空"})
			return
		}
		s.accountMgr.SetModelAccounts(body.Model, body.AccountIDs)
		if s.DB != nil {
			s.DB.SaveModelAccounts(body.Model, body.AccountIDs)
		}
		log.Printf("[Admin] 设置模型 %s 的账号: %v", body.Model, body.AccountIDs)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "model": body.Model, "accounts": len(body.AccountIDs)})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleModelAccountsAction DELETE /api/model-accounts/{model}
func (s *Server) handleModelAccountsAction(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimPrefix(r.URL.Path, "/api/model-accounts/")
	if model == "" {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "缺少模型名"})
		return
	}
	if r.Method == "DELETE" {
		s.accountMgr.SetModelAccounts(model, nil)
		if s.DB != nil {
			s.DB.DeleteModelAccounts(model)
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
	} else {
		http.Error(w, "Method not allowed", 405)
	}
}

// handleProxyAction 的后续部分（toggle/test/bind/unbind）
func (s *Server) handleProxySubAction(w http.ResponseWriter, r *http.Request, proxyID string, action string) {
	switch action {
	case "toggle":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		info, ok := s.proxyMgr.ToggleProxy(proxyID)
		if ok {
			if s.DB != nil {
				s.DB.SaveProxy(info)
			}
			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "enabled": info.Enabled})
		} else {
			httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Proxy not found"})
		}
	case "test":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		proxies := s.proxyMgr.GetAllProxies()
		var target *legacy.ProxyInfo
		for _, p := range proxies {
			if p.ID == proxyID {
				target = p
				break
			}
		}
		if target == nil {
			httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Proxy not found"})
			return
		}
		transport := s.proxyMgr.BuildTransport(target)
		if transport == nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Failed to build transport"})
			return
		}
		client := &http.Client{Timeout: 15 * time.Second, Transport: transport}

		// Try HTTPS first, fallback to HTTP
		testURLs := []string{"https://httpbin.org/ip", "http://httpbin.org/ip"}
		var lastErr error
		var elapsed time.Duration
		var ipResp map[string]interface{}

		for _, testURL := range testURLs {
			start := time.Now()
			resp, err := client.Get(testURL)
			elapsed = time.Since(start)
			if err != nil {
				lastErr = err
				continue
			}
			json.NewDecoder(resp.Body).Decode(&ipResp)
			resp.Body.Close()
			lastErr = nil
			break
		}

		if lastErr != nil {
			errMsg := lastErr.Error()
			s.proxyMgr.RecordError(target, errMsg)
			httputil.WriteJSON(w, 200, map[string]interface{}{
				"ok":      false,
				"error":   errMsg,
				"latency": elapsed.Milliseconds(),
			})
			return
		}

		s.proxyMgr.RecordSuccess(target)
		ip, _ := ipResp["origin"].(string)
		s.proxyMgr.UpdateProxy(target.ID, func(p *legacy.ProxyInfo) {
			p.LastLatency = elapsed.Milliseconds()
			p.LastTestIP = ip
		})
		httputil.WriteJSON(w, 200, map[string]interface{}{
		"ok":      true,
		"ip":      ip,
		"latency": elapsed.Milliseconds(),
		})
	default:
		http.NotFound(w, r)
	}
}

// handleTestAllProxies tests all proxies concurrently and returns results
func (s *Server) handleTestAllProxies(w http.ResponseWriter, r *http.Request) {
	proxies := s.proxyMgr.GetAllProxies()
	if len(proxies) == 0 {
		httputil.WriteJSON(w, 200, map[string]interface{}{"results": []interface{}{}, "total": 0})
		return
	}

	type testResult struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		IP      string `json:"ip"`
		Latency int64  `json:"latency_ms"`
		Error   string `json:"error,omitempty"`
	}

	results := make([]testResult, len(proxies))
	var wg sync.WaitGroup

	for i, p := range proxies {
		wg.Add(1)
		go func(idx int, px *legacy.ProxyInfo) {
			defer wg.Done()
			res := testResult{ID: px.ID, Name: px.Name}

			if !px.Enabled {
				res.Error = "已禁用"
				results[idx] = res
				return
			}

			transport := s.proxyMgr.BuildTransport(px)
			if transport == nil {
				res.Error = "构建连接失败"
				s.proxyMgr.RecordError(px, res.Error)
				results[idx] = res
				return
			}

			client := &http.Client{Timeout: 15 * time.Second, Transport: transport}

			// Try HTTPS first, fallback to HTTP
			var lastErr error
			var ipResp map[string]interface{}
			for _, testURL := range []string{"https://httpbin.org/ip", "http://httpbin.org/ip"} {
				start := time.Now()
				resp, err := client.Get(testURL)
				res.Latency = time.Since(start).Milliseconds()
				if err != nil {
					lastErr = err
					continue
				}
				json.NewDecoder(resp.Body).Decode(&ipResp)
				resp.Body.Close()
				lastErr = nil
				break
			}

			if lastErr != nil {
				res.Error = lastErr.Error()
				s.proxyMgr.RecordError(px, res.Error)
				results[idx] = res
				return
			}

			ip, _ := ipResp["origin"].(string)

			res.OK = true
			res.IP = ip
			s.proxyMgr.RecordSuccess(px)

			s.proxyMgr.UpdateProxy(px.ID, func(p *legacy.ProxyInfo) {
				p.LastLatency = res.Latency
				p.LastTestIP = ip
			})

			results[idx] = res
		}(i, p)
	}

	wg.Wait()

	okCount := 0
	failCount := 0
	for _, res := range results {
		if res.OK {
			okCount++
		} else {
			failCount++
		}
	}

	httputil.WriteJSON(w, 200, map[string]interface{}{
		"results": results,
		"total":   len(results),
		"ok":      okCount,
		"fail":    failCount,
	})
}

// handleUpdateProxy updates a proxy's settings
func (s *Server) handleUpdateProxy(w http.ResponseWriter, r *http.Request, proxyID string) {
	var body struct {
		Name        *string `json:"name"`
		URL         *string `json:"url"`
		Type        *string `json:"type"`
		MaxAccounts *int    `json:"max_accounts"`
		ExpiresAt   *string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	ok := s.proxyMgr.UpdateProxy(proxyID, func(p *legacy.ProxyInfo) {
		if body.Name != nil {
			p.Name = *body.Name
		}
		if body.URL != nil && *body.URL != "" {
			// Support pipe format in edit too
			if !strings.Contains(*body.URL, "://") && strings.Contains(*body.URL, "|") {
				if parsed := legacy.ParseProxyLine(*body.URL); parsed != nil {
					p.URL = parsed.URL
					if p.Name == "" || (body.Name == nil) {
						p.Name = parsed.Name
					}
					if parsed.ExpiresAt != "" && body.ExpiresAt == nil {
						p.ExpiresAt = parsed.ExpiresAt
					}
					if parsed.Type != "" && body.Type == nil {
						p.Type = parsed.Type
					}
					return
				}
			}
			p.URL = *body.URL
		}
		if body.Type != nil {
			p.Type = *body.Type
		}
		if body.MaxAccounts != nil {
			p.MaxAccounts = *body.MaxAccounts
		}
		if body.ExpiresAt != nil {
			p.ExpiresAt = *body.ExpiresAt
		}
	})

	if ok {
		// 保存到数据库
		if s.DB != nil {
			for _, p := range s.proxyMgr.GetAllProxies() {
				if p.ID == proxyID {
					s.DB.SaveProxy(p)
					break
				}
			}
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
	} else {
		httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Proxy not found"})
	}
}

// handleAccountUsageLimits queries Kiro API for account usage/credits
func (s *Server) handleAccountUsageLimits(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			token := acc.GetToken()
			if token == "" {
				httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Account has no token"})
				return
			}
			machineID := acc.GetMachineID()

			// Use account-bound proxy, fallback to direct
			var transport http.RoundTripper
			if s.proxyMgr != nil {
				transport = s.proxyMgr.GetTransportForAccount(acc.ID)
			}

			usage, err := s.kiroClient.GetUsageLimits(token, machineID, transport)
			// If proxy failed, retry without proxy
			if err != nil && transport != nil {
				log.Printf("[UsageLimits] 代理失败，回退直连: %s", kiro.TruncStr(err.Error(), 100))
				usage, err = s.kiroClient.GetUsageLimits(token, machineID)
			}
			if err != nil {
				httputil.WriteJSON(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}

			// Store on account
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

			// 保存到数据库
			if s.DB != nil {
				SaveAccountToDB(s.DB, acc)
			}

			log.Printf("[UsageLimits] %s: %s 用量=%.1f/%.1f 试用=%.1f/%.1f (%s)",
				acc.Email, usage.SubscriptionTitle,
				usage.CurrentUsage, usage.UsageLimit,
				usage.FreeTrialUsage, usage.FreeTrialLimit,
				usage.FreeTrialStatus)

			httputil.WriteJSON(w, 200, map[string]interface{}{
				"ok":    true,
				"usage": acc.UsageLimits,
			})
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// handleEditAccount updates account fields
func (s *Server) handleEditAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != "PUT" && r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var body struct {
		Email           *string  `json:"email"`
		Nickname        *string  `json:"nickname"`
		MaxConcurrent   *int     `json:"max_concurrent"`
		SupportedModels []string `json:"supported_models"`
		ProxyID         *string  `json:"proxy_id"`
		MachineID       *string  `json:"machine_id"`
		AccessToken     *string  `json:"access_token"`
		RefreshToken    *string  `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	accounts := s.accountMgr.GetAllAccounts()
	for _, acc := range accounts {
		if acc.ID == accountID {
			acc.Mu.Lock()

			if body.Email != nil && *body.Email != "" {
				acc.Email = *body.Email
			}
			if body.Nickname != nil {
				acc.Nickname = *body.Nickname
			}
			if body.MaxConcurrent != nil {
				mc := *body.MaxConcurrent
				if mc < 1 {
					mc = kiro.DefaultMaxConcurrent
				}
				acc.MaxConcurrent = mc
			}
			if body.SupportedModels != nil {
				acc.SupportedModels = body.SupportedModels
			}
			if body.ProxyID != nil {
				oldProxyID := acc.ProxyID
				newProxyID := *body.ProxyID

				// Unbind old proxy
				if oldProxyID != "" && oldProxyID != newProxyID {
					s.proxyMgr.UnbindAccount(oldProxyID, acc.ID)
				}
				// Bind new proxy
				if newProxyID != "" && newProxyID != oldProxyID {
					if ok := s.proxyMgr.BindAccount(newProxyID, acc.ID); !ok {
						acc.Mu.Unlock()
						httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Proxy not found or full"})
						return
					}
				}
				acc.ProxyID = newProxyID
			}
			if body.MachineID != nil && *body.MachineID != "" {
				acc.MachineID = *body.MachineID
			}
			if body.AccessToken != nil && *body.AccessToken != "" {
				if acc.Credentials == nil {
					acc.Credentials = &kiro.KiroCredentials{}
				}
				acc.Credentials.AccessToken = *body.AccessToken
			}
			if body.RefreshToken != nil && *body.RefreshToken != "" {
				if acc.Credentials == nil {
					acc.Credentials = &kiro.KiroCredentials{}
				}
				acc.Credentials.RefreshToken = *body.RefreshToken
			}

			acc.Mu.Unlock()

			// 保存到数据库
			if s.DB != nil {
				SaveAccountToDB(s.DB, acc)
			}

			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
			return
		}
	}
	httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Account not found"})
}

// ==================== Key Management API ====================

// handleKeys handles GET (list) and POST (create) for API keys
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		keys := s.keyMgr.GetAllKeys()
		result := make([]map[string]interface{}, 0, len(keys))
		for _, k := range keys {
			result = append(result, map[string]interface{}{
				"key":         k.Key,
				"name":        k.Name,
				"created_at":  k.CreatedAt,
				"last_used_at": k.LastUsedAt,
				"enabled":     k.Enabled,
				"rate_limit":  k.RateLimit,
				"total_usage": k.TotalUsage,
				"description": k.Description,
			})
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{"keys": result})

	case "POST":
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			RateLimit   int    `json:"rate_limit"`
			Key         string `json:"custom_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		if body.Name == "" {
			body.Name = "Unnamed Key"
		}
		info := s.keyMgr.CreateKey(body.Name, body.Description, body.RateLimit, body.Key)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "key": info.Key})

	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleKeyAction handles individual key operations: toggle, delete
func (s *Server) handleKeyAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	keyID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "toggle":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		info, ok := s.keyMgr.ToggleKey(keyID)
		if ok {
			httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "enabled": info.Enabled})
		} else {
			httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Key not found"})
		}
	default:
		// No sub-action: DELETE the key
		if r.Method == "DELETE" {
			if s.keyMgr.DeleteKey(keyID) {
				httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
			} else {
				httputil.WriteJSON(w, 404, map[string]interface{}{"error": "Key not found"})
			}
		} else {
			http.Error(w, "Method not allowed", 405)
		}
	}
}

// handleKiroModelsSettings GET/PUT available Kiro models list
func (s *Server) handleKiroModelsSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"models": kiro.KiroModels,
		})
	case "PUT", "POST":
		var body struct {
			Models []string `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		var clean []string
		for _, m := range body.Models {
			m = strings.TrimSpace(m)
			if m != "" {
				clean = append(clean, m)
			}
		}
		kiro.KiroModels = clean
		if s.DB != nil {
			s.DB.SetSetting("kiro_models", strings.Join(clean, "\n"))
		}
		// Clean up: remove deleted models from account SupportedModels and model_accounts mapping
		validSet := make(map[string]bool)
		for _, m := range clean {
			validSet[strings.ToLower(m)] = true
		}
		for _, acc := range s.accountMgr.GetAllAccounts() {
			if len(acc.SupportedModels) > 0 {
				var kept []string
				for _, m := range acc.SupportedModels {
					if validSet[strings.ToLower(m)] {
						kept = append(kept, m)
					}
				}
				if len(kept) != len(acc.SupportedModels) {
					acc.Mu.Lock()
					acc.SupportedModels = kept
					acc.Mu.Unlock()
					if s.DB != nil && acc.OnStatusChange != nil {
						acc.OnStatusChange(acc)
					}
				}
			}
		}
		// Clean model_accounts mapping
		allMapping := s.accountMgr.GetAllModelAccounts()
		for model := range allMapping {
			if !validSet[strings.ToLower(model)] {
				s.accountMgr.SetModelAccounts(model, nil)
				if s.DB != nil {
					s.DB.SaveModelAccounts(model, nil)
				}
			}
		}
		log.Printf("[Models] 更新 Kiro 模型列表: %v", clean)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "models": clean})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// ==================== Usage & Stats API ====================

// handleUsage returns usage summary
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, 200, map[string]interface{}{
		"total":      s.usageTracker.GetTotalSummary(),
		"by_account": s.usageTracker.GetSummaryByAccount(),
		"by_model":   s.usageTracker.GetSummaryByModel(),
		"by_key":     s.usageTracker.GetSummaryByKey(),
	})
}

// handleUsageRecords returns recent usage records
func (s *Server) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	n := 100
	if qn := r.URL.Query().Get("limit"); qn != "" {
		if parsed, err := fmt.Sscanf(qn, "%d", &n); err != nil || parsed == 0 {
			n = 100
		}
	}
	if n > 1000 {
		n = 1000
	}
	records := s.usageTracker.GetRecentRecords(n)
	httputil.WriteJSON(w, 200, map[string]interface{}{"records": records})
}

// handleLogs returns recent request/response log summaries, or a single log detail
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	// 单条详情: GET /api/logs?file=20260404/2307/230701_anthropic_0001.json
	if fileKey := r.URL.Query().Get("file"); fileKey != "" {
		detail, err := GetRequestLogDetail(fileKey)
		if err != nil {
			httputil.WriteJSON(w, 404, map[string]interface{}{"error": err.Error()})
			return
		}
		httputil.WriteJSON(w, 200, detail)
		return
	}

	// 列表: GET /api/logs?limit=30
	n := 30
	if qn := r.URL.Query().Get("limit"); qn != "" {
		if parsed, err := fmt.Sscanf(qn, "%d", &n); err != nil || parsed == 0 {
			n = 30
		}
	}
	if n > 200 {
		n = 200
	}
	logs := ListRequestLogs(n)
	httputil.WriteJSON(w, 200, map[string]interface{}{"logs": logs, "count": len(logs)})
}

// handleStats returns rate limiter and system stats
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.rateLimiter.GetStats()
	stats["usage"] = s.usageTracker.GetTotalSummary()
	stats["accounts"] = s.accountMgr.GetAccountStats()
	httputil.WriteJSON(w, 200, stats)
}

// ==================== Settings API ====================

// handleRateLimitSettings handles GET/PUT for rate limit configuration
func (s *Server) handleRateLimitSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := s.rateLimiter.GetConfig()
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"enabled":                        cfg.Enabled,
			"min_request_interval":           cfg.MinRequestInterval,
			"max_requests_per_minute":        cfg.MaxRequestsPerMinute,
			"global_max_requests_per_minute": cfg.GlobalMaxRequestsPerMinute,
			"quota_cooldown_seconds":         cfg.QuotaCooldownSeconds,
			"retry_timeout_seconds":          cfg.RetryTimeoutSeconds,
			"retry_429_delay_seconds":        cfg.Retry429DelaySeconds,
			"retry_max_attempts":             cfg.RetryMaxAttempts,
			"retry_error_delay_seconds":      cfg.RetryErrorDelaySeconds,
			"cooldown_threshold":             cfg.CooldownThreshold,
			"connect_timeout_seconds":        cfg.ConnectTimeoutSeconds,
		})

	case "PUT", "POST":
		var body struct {
			Enabled                    *bool    `json:"enabled"`
			MinRequestInterval         *float64 `json:"min_request_interval"`
			MaxRequestsPerMinute       *int     `json:"max_requests_per_minute"`
			GlobalMaxRequestsPerMinute *int     `json:"global_max_requests_per_minute"`
			QuotaCooldownSeconds       *int     `json:"quota_cooldown_seconds"`
			RetryTimeoutSeconds        *int     `json:"retry_timeout_seconds"`
			Retry429DelaySeconds       *float64 `json:"retry_429_delay_seconds"`
			RetryMaxAttempts           *int     `json:"retry_max_attempts"`
			RetryErrorDelaySeconds     *float64 `json:"retry_error_delay_seconds"`
			CooldownThreshold          *int     `json:"cooldown_threshold"`
			ConnectTimeoutSeconds      *int     `json:"connect_timeout_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		s.rateLimiter.UpdateConfigFields(func(cfg *legacy.RateLimitConfig) {
			if body.Enabled != nil {
				cfg.Enabled = *body.Enabled
			}
			if body.MinRequestInterval != nil {
				cfg.MinRequestInterval = *body.MinRequestInterval
			}
			if body.MaxRequestsPerMinute != nil {
				cfg.MaxRequestsPerMinute = *body.MaxRequestsPerMinute
			}
			if body.GlobalMaxRequestsPerMinute != nil {
				cfg.GlobalMaxRequestsPerMinute = *body.GlobalMaxRequestsPerMinute
			}
			if body.QuotaCooldownSeconds != nil {
				cfg.QuotaCooldownSeconds = *body.QuotaCooldownSeconds
			}
			if body.RetryTimeoutSeconds != nil {
				cfg.RetryTimeoutSeconds = *body.RetryTimeoutSeconds
			}
			if body.Retry429DelaySeconds != nil {
				cfg.Retry429DelaySeconds = *body.Retry429DelaySeconds
			}
			if body.RetryMaxAttempts != nil {
				cfg.RetryMaxAttempts = *body.RetryMaxAttempts
			}
			if body.RetryErrorDelaySeconds != nil {
				cfg.RetryErrorDelaySeconds = *body.RetryErrorDelaySeconds
			}
			if body.CooldownThreshold != nil {
				cfg.CooldownThreshold = *body.CooldownThreshold
			}
			if body.ConnectTimeoutSeconds != nil {
				cfg.ConnectTimeoutSeconds = *body.ConnectTimeoutSeconds
			}
		})
		// Persist to DB
		if s.DB != nil {
			s.rateLimiter.SaveConfigToDB(s.DB)
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})

	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleDebugSave toggles request/response body saving to disk.
// GET returns current status, POST toggles it.
func (s *Server) handleDebugSave(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"enabled": kiro.IsDebugSaveEnabled(),
		})
	case "POST":
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := httputil.ReadJSON(r, &body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "invalid request"})
			return
		}
		kiro.SetDebugSave(body.Enabled)
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"ok":      true,
			"enabled": kiro.IsDebugSaveEnabled(),
		})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleConcurrentSettings handles GET/PUT for per-account max concurrent requests
func (s *Server) handleConcurrentSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		accounts := s.accountMgr.GetAllAccounts()
		defaultMax := kiro.DefaultMaxConcurrent
		// Read persisted value from DB
		if s.DB != nil {
			if v := s.DB.GetSetting("default_max_concurrent"); v != "" {
				var n int
				if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 100 {
					defaultMax = n
				}
			}
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"default_max_concurrent": defaultMax,
			"total_accounts":         len(accounts),
		})
	case "PUT", "POST":
		var body struct {
			MaxConcurrent int `json:"max_concurrent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		if body.MaxConcurrent < 1 {
			body.MaxConcurrent = 1
		}
		if body.MaxConcurrent > 100 {
			body.MaxConcurrent = 100
		}
		accounts := s.accountMgr.GetAllAccounts()
		updated := 0
		for _, acc := range accounts {
			acc.Mu.Lock()
			acc.MaxConcurrent = body.MaxConcurrent
			acc.Mu.Unlock()
			if s.DB != nil {
				SaveAccountToDB(s.DB, acc)
			}
			updated++
		}
		log.Printf("[Admin] 批量设置并发限制: max_concurrent=%d, 更新了 %d 个账号", body.MaxConcurrent, updated)
		// Persist default value to settings table
		if s.DB != nil {
			s.DB.SetSetting("default_max_concurrent", fmt.Sprintf("%d", body.MaxConcurrent))
		}
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"ok":              true,
			"max_concurrent":  body.MaxConcurrent,
			"updated":         updated,
		})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleModelStripSettings GET/PUT model name strip patterns
func (s *Server) handleModelStripSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"patterns": kiro.ModelStripPatterns,
		})
	case "PUT", "POST":
		var body struct {
			Patterns []string `json:"patterns"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		var clean []string
		for _, p := range body.Patterns {
			p = strings.TrimSpace(p)
			if p != "" {
				clean = append(clean, p)
			}
		}
		kiro.ModelStripPatterns = clean
		if s.DB != nil {
			s.DB.SetSetting("model_strip_patterns", strings.Join(clean, "\n"))
		}
		log.Printf("[Models] 更新模型名清理规则: %d 条", len(clean))
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "patterns": clean})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleModelAliasesSettings GET/PUT model name aliases (input -> actual)
func (s *Server) handleModelAliasesSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		// Convert map to lines for display
		var lines []string
		for from, to := range kiro.ModelAliases {
			lines = append(lines, from+"="+to)
		}
		sort.Strings(lines)
		httputil.WriteJSON(w, 200, map[string]interface{}{
			"aliases": kiro.ModelAliases,
			"text":    strings.Join(lines, "\n"),
		})
	case "PUT", "POST":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		aliases := make(map[string]string)
		for _, line := range strings.Split(body.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.Contains(line, "=") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			from := strings.TrimSpace(parts[0])
			to := strings.TrimSpace(parts[1])
			if from != "" && to != "" {
				aliases[strings.ToLower(from)] = to
			}
		}
		kiro.ModelAliases = aliases
		if s.DB != nil {
			var lines []string
			for from, to := range aliases {
				lines = append(lines, from+"="+to)
			}
			s.DB.SetSetting("model_aliases", strings.Join(lines, "\n"))
		}
		log.Printf("[Models] 更新模型映射: %d 条", len(aliases))
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "count": len(aliases)})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// handleAutoReply 管理自动回复规则
func (s *Server) handleAutoReply(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		httputil.WriteJSON(w, 200, s.AutoReply.GetRules())
	case "POST":
		var rule AutoReply
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		if rule.Keyword == "" {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "keyword 不能为空"})
			return
		}
		s.AutoReply.AddRule(rule)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
	case "PUT":
		var rules []AutoReply
		if err := json.NewDecoder(r.Body).Decode(&rules); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		s.AutoReply.SetRules(rules)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true, "count": len(rules)})
	case "DELETE":
		var body struct {
			Index int `json:"index"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httputil.WriteJSON(w, 400, map[string]interface{}{"error": "Invalid JSON"})
			return
		}
		s.AutoReply.DeleteRule(body.Index)
		httputil.WriteJSON(w, 200, map[string]interface{}{"ok": true})
	default:
		http.Error(w, "Method not allowed", 405)
	}
}