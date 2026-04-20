package proxy

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
)

// ==================== Account ====================

type Account struct {
	// int64 fields must be first for 64-bit alignment on 32-bit systems
	RequestCount      int64
	ErrorCount        int64
	ActiveRequests    int64 // current in-flight requests
	TotalRequests429  int64 // total 429 count
	TotalRequestsOK  int64 // total success count
	TotalRequestsErr int64 // total non-429 error count
	OnStatusChange   func(acc *Account) // 状态变化时的回调（用于持久化）
	ID               string
	Email            string
	Nickname         string
	IDP              string // identity provider (Google, GitHub, etc.)
	MachineID        string // 每个账号独立的 machineId，持久化存储
	Credentials      *kiro.KiroCredentials
	Status           kiro.CredentialStatus
	LastUsedAt       time.Time
	Enabled          bool

	// Concurrency & health
	MaxConcurrent    int       // max simultaneous requests, 0 = default (2)
	ConsecutiveErrs  int       // consecutive non-429 errors
	CooldownUntil    time.Time // if set, account is in cooldown
	RateLimitedUntil time.Time // short cooldown after 429
	Recent429Count   int       // 429 count in recent window
	Last429At        time.Time // last 429 timestamp
	SuspendedAt      time.Time // when account was suspended
	SuspendedReason  string    // why it was suspended
	LastErrorCode    int       // last HTTP error code
	LastErrorMessage string    // last error message
	LastSuccessAt    time.Time // last successful request
	ProxyID          string    // bound proxy ID

	// Real-time request status tracking
	LastRequestTime     time.Time     // when the last request started
	LastRequestStatus   string        // "streaming", "success", "429", "error", "EOF"
	LastRequestDuration time.Duration // how long the last request took

	// Kiro credits tracking
	CreditsUsed         float64 // total credits consumed (from meteringEvent)
	LastCreditsUsed     float64 // credits used in last request
	ContextUsagePercent float64 // last context usage percentage

	// Kiro usage limits (from getUsageLimits API)
	UsageLimits *kiro.KiroUsageLimits

	// 支持的模型列表，空表示支持所有模型
	SupportedModels []string

	// slot 释放通知回调（由 AccountManager 设置）
	onSlotRelease func()

	Mu sync.Mutex
}

// GetCooldownThreshold returns the configured threshold from GlobalRateLimiter
func GetCooldownThreshold() int {
	cfg := legacy.GlobalRateLimiter.GetConfig()
	if cfg.CooldownThreshold > 0 {
		return cfg.CooldownThreshold
	}
	return 10
}

func (a *Account) GetMaxConcurrent() int {
	if a.MaxConcurrent > 0 {
		return a.MaxConcurrent
	}
	return kiro.DefaultMaxConcurrent
}

func (a *Account) IsAvailable() bool {
	a.Mu.Lock()
	defer a.Mu.Unlock()

	if !a.Enabled {
		return false
	}
	if a.Status == kiro.StatusDisabled || a.Status == kiro.StatusSuspended {
		return false
	}
	// Check cooldown — 过期自动重置
	if a.Status == kiro.StatusCooldown {
		if time.Now().Before(a.CooldownUntil) {
			return false
		}
		a.Status = kiro.StatusActive
		a.ConsecutiveErrs = 0
	}
	// Check 429 rate limit cooldown
	if !a.RateLimitedUntil.IsZero() && time.Now().Before(a.RateLimitedUntil) {
		return false
	}
	// Check usage limits — 额度用完不再分配
	if a.UsageLimits != nil && a.UsageLimits.UsageLimit > 0 {
		if a.UsageLimits.CurrentUsage >= a.UsageLimits.UsageLimit {
			return false
		}
	}
	return true
}

// SupportsModel 检查账号是否支持指定模型
// 优先从 AccountManager.ModelAccounts 查找，如果没有配置则所有账号都可用
func (a *Account) SupportsModel(model string) bool {
	if len(a.SupportedModels) > 0 {
		for _, m := range a.SupportedModels {
			if strings.EqualFold(m, model) {
				return true
			}
		}
		return false
	}
	return true
}

// AcquireSlot tries to acquire a request slot. Returns true if successful.
func (a *Account) AcquireSlot() bool {
	for {
		current := atomic.LoadInt64(&a.ActiveRequests)
		limit := int64(a.GetMaxConcurrent())
		if current >= limit {
			return false
		}
		if atomic.CompareAndSwapInt64(&a.ActiveRequests, current, current+1) {
			return true
		}
	}
}

// ReleaseSlot releases a request slot after completion.
func (a *Account) ReleaseSlot() {
	for {
		current := atomic.LoadInt64(&a.ActiveRequests)
		if current <= 0 {
			return // prevent going negative
		}
		if atomic.CompareAndSwapInt64(&a.ActiveRequests, current, current-1) {
			if a.onSlotRelease != nil {
				a.onSlotRelease()
			}
			return
		}
	}
}

// RecordSuccess resets consecutive error counter
func (a *Account) RecordSuccess() {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.ConsecutiveErrs = 0
	a.LastErrorCode = 0
	a.LastErrorMessage = ""
	a.LastSuccessAt = time.Now()
	a.Recent429Count = 0
	a.RateLimitedUntil = time.Time{} // clear 429 cooldown on success
	a.LastRequestStatus = "success"
	atomic.AddInt64(&a.TotalRequestsOK, 1)
}

// SetRequestStatus sets the real-time request status (streaming, success, 429, error, EOF)
func (a *Account) SetRequestStatus(status string) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.LastRequestStatus = status
	a.LastRequestTime = time.Now()
}

// SetRequestDone marks the request as done with final status and duration
func (a *Account) SetRequestDone(status string, duration time.Duration) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.LastRequestStatus = status
	a.LastRequestDuration = duration
	a.LastRequestTime = time.Now()
}

// RecordCredits records credits consumed from a Kiro response.
// If OnStatusChange is set, it will be called to persist the updated credits to DB.
func (a *Account) RecordCredits(creditsUsed, contextPct float64) {
	a.Mu.Lock()
	changed := false
	if creditsUsed > 0 {
		a.CreditsUsed += creditsUsed
		a.LastCreditsUsed = creditsUsed
		// 同步更新 UsageLimits 的 CurrentUsage
		if a.UsageLimits != nil {
			a.UsageLimits.CurrentUsage += creditsUsed
		}
		changed = true
	}
	if contextPct > 0 {
		a.ContextUsagePercent = contextPct
		changed = true
	}
	cb := a.OnStatusChange
	a.Mu.Unlock()
	if changed && cb != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Account] panic in OnStatusChange callback: %v", r)
				}
			}()
			cb(a)
		}()
	}
}

// RecordError processes an error response and may suspend/cooldown the account.
// If db is provided and the account is suspended, it will be persisted.
func (a *Account) RecordError(statusCode int, errMsg string) {
	a.Mu.Lock()
	defer a.Mu.Unlock()

	atomic.AddInt64(&a.ErrorCount, 1)
	a.LastErrorCode = statusCode
	a.LastErrorMessage = errMsg

	// Check if this looks like a ban/suspension
	if kiro.IsSuspensionError(statusCode, errMsg) {
		a.Status = kiro.StatusSuspended
		a.SuspendedAt = time.Now()
		a.SuspendedReason = errMsg
		a.LastRequestStatus = "error"
		atomic.AddInt64(&a.TotalRequestsErr, 1)
		log.Printf("[Account] ⛔ 账号 %s 疑似被封禁 (HTTP %d: %s)，已标记为暂停", a.Email, statusCode, kiro.TruncStr(errMsg, 100))
		if a.OnStatusChange != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[Account] panic in OnStatusChange callback: %v", r)
					}
				}()
				a.OnStatusChange(a)
			}()
		}
		return
	}

	// 429 不触发冷却，由重试逻辑处理
	if statusCode == 429 {
		a.LastRequestStatus = "429"
		atomic.AddInt64(&a.TotalRequests429, 1)
		return
	}

	a.LastRequestStatus = "error"
	atomic.AddInt64(&a.TotalRequestsErr, 1)
	a.ConsecutiveErrs++
	threshold := GetCooldownThreshold()
	if a.ConsecutiveErrs >= threshold {
		a.Status = kiro.StatusCooldown
		a.CooldownUntil = time.Now().Add(2 * time.Minute)
		log.Printf("[Account] ⏸️ 账号 %s 连续 %d 次错误，进入冷却 2m", a.Email, a.ConsecutiveErrs)
	}
}

func (a *Account) GetToken() string {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.Credentials != nil {
		return a.Credentials.AccessToken
	}
	return ""
}

func (a *Account) GetMachineID() string {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.MachineID != "" {
		return a.MachineID
	}
	// 首次调用时生成并持久化
	a.MachineID = kiro.GenerateRandomMachineID()
	return a.MachineID
}

func (a *Account) GetProfileArn() string {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.Credentials != nil {
		return a.Credentials.ProfileArn
	}
	return ""
}

func (a *Account) RefreshToken(transport ...http.RoundTripper) (bool, string) {
	a.Mu.Lock()
	if a.Credentials == nil {
		a.Mu.Unlock()
		return false, "no credentials"
	}
	// Copy credentials for network I/O outside the lock
	credsCopy := *a.Credentials
	a.Mu.Unlock()

	refresher := &kiro.TokenRefresher{Creds: &credsCopy}
	if len(transport) > 0 && transport[0] != nil {
		refresher.Transport = transport[0]
	}
	ok, result := refresher.Refresh()

	// Re-lock to update state
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if ok {
		// Apply refreshed credentials back
		a.Credentials.AccessToken = credsCopy.AccessToken
		a.Credentials.RefreshToken = credsCopy.RefreshToken
		a.Credentials.ExpiresAt = credsCopy.ExpiresAt
		a.Credentials.LastRefresh = credsCopy.LastRefresh
		if credsCopy.ProfileArn != "" {
			a.Credentials.ProfileArn = credsCopy.ProfileArn
		}
		a.Status = kiro.StatusActive
		a.ConsecutiveErrs = 0
	} else {
		// Check if refresh failure indicates suspension
		if kiro.IsSuspensionError(0, result) {
			a.Status = kiro.StatusSuspended
			a.SuspendedAt = time.Now()
			a.SuspendedReason = "Token refresh failed: " + result
			log.Printf("[Account] ⛔ 账号 %s Token刷新失败疑似封禁: %s", a.Email, kiro.TruncStr(result, 100))
		} else {
			log.Printf("[Account] ⚠️ 账号 %s Token刷新失败: %s", a.Email, kiro.TruncStr(result, 100))
		}
	}
	return ok, result
}

// Unsuspend manually reactivates a suspended account
func (a *Account) Unsuspend() {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.Status = kiro.StatusActive
	a.ConsecutiveErrs = 0
	a.SuspendedReason = ""
	a.CooldownUntil = time.Time{}
	log.Printf("[Account] ✅ 账号 %s 已手动解除暂停", a.Email)
}

// GetID returns the account ID (implements legacy.AccountLike)
func (a *Account) GetID() string { return a.ID }

// GetProxyID returns the proxy ID (implements legacy.AccountLike)
func (a *Account) GetProxyID() string { return a.ProxyID }

// ==================== AccountManager ====================

type AccountManager struct {
	// roundRobin must be first field for 64-bit alignment on 32-bit systems
	roundRobin   uint64
	Accounts     []*Account
	Mu           sync.RWMutex
	accountsFile string
	proxyMgr     *legacy.ProxyManager
	db           *legacy.Database

	// Session stickiness: map[sessionID] -> accountID + timestamp
	sessions   map[string]*sessionEntry
	sessionsMu sync.Mutex

	// 模型-账号映射: map[modelId] -> []accountID
	// 空 map 表示所有账号都可用于所有模型
	ModelAccounts   map[string][]string
	modelAccountsMu sync.RWMutex

	// 等待队列通知：slot 释放时广播
	slotNotify chan struct{}

	// 全局排队计数
	QueuedRequests int64

	// 每个账号的排队计数: map[accountID] -> queued count
	accountQueued   map[string]*int64
	accountQueuedMu sync.RWMutex
}

type sessionEntry struct {
	accountID string
	lastUsed  time.Time
}

func NewAccountManager(accountsFilePath string) *AccountManager {
	am := &AccountManager{
		accountsFile:  accountsFilePath,
		sessions:      make(map[string]*sessionEntry),
		ModelAccounts: make(map[string][]string),
		slotNotify:    make(chan struct{}, 1),
		accountQueued: make(map[string]*int64),
	}
	return am
}

// notifySlot 非阻塞地向等待队列发送 slot 释放通知
func (am *AccountManager) notifySlot() {
	select {
	case am.slotNotify <- struct{}{}:
	default:
	}
}

// AddAccount appends an account to the manager (thread-safe).
func (am *AccountManager) AddAccount(acc *Account) {
	// 设置 slot 释放通知
	acc.onSlotRelease = func() {
		am.notifySlot()
	}
	am.Mu.Lock()
	am.Accounts = append(am.Accounts, acc)
	am.Mu.Unlock()
}

// SetupSlotNotify 为所有已加载的账号设置 slot 释放通知回调
func (am *AccountManager) SetupSlotNotify() {
	am.Mu.RLock()
	defer am.Mu.RUnlock()
	for _, acc := range am.Accounts {
		acc.onSlotRelease = func() {
			am.notifySlot()
		}
	}
}

// RemoveAccount removes an account by ID (thread-safe). Returns true if found.
func (am *AccountManager) RemoveAccount(accountID string) bool {
	am.Mu.Lock()
	defer am.Mu.Unlock()
	for i, acc := range am.Accounts {
		if acc.ID == accountID {
			am.Accounts = append(am.Accounts[:i], am.Accounts[i+1:]...)
			return true
		}
	}
	return false
}

// HasToken 检查是否已存在相同 accessToken 的账号
func (am *AccountManager) HasToken(token string) bool {
	am.Mu.RLock()
	defer am.Mu.RUnlock()
	for _, acc := range am.Accounts {
		if acc.GetToken() == token {
			return true
		}
	}
	return false
}

// HasClientID 检查是否已存在相同 clientId 的账号
func (am *AccountManager) HasClientID(clientID string) bool {
	if clientID == "" {
		return false
	}
	am.Mu.RLock()
	defer am.Mu.RUnlock()
	for _, acc := range am.Accounts {
		if acc.Credentials != nil && acc.Credentials.ClientID == clientID {
			return true
		}
	}
	return false
}

// HasEmail 检查是否已存在相同 email 的账号
func (am *AccountManager) HasEmail(email string) bool {
	am.Mu.RLock()
	defer am.Mu.RUnlock()
	for _, acc := range am.Accounts {
		if acc.Email == email {
			return true
		}
	}
	return false
}

func (am *AccountManager) LoadAccounts() error {
	af, err := kiro.LoadAccountsFromFile(am.accountsFile)
	if err != nil {
		return err
	}

	am.Mu.Lock()
	defer am.Mu.Unlock()

	am.Accounts = nil
	for _, acc := range af.Accounts {
		if acc.Status != "active" {
			log.Printf("[AccountManager] Skipping inactive account: %s (%s)", acc.Email, acc.Status)
			continue
		}

		creds := acc.Credentials
		account := &Account{
			ID:          acc.ID,
			Email:       acc.Email,
			Nickname:    acc.Nickname,
			MachineID:   kiro.GenerateRandomMachineID(),
			Credentials: &creds,
			Status:      kiro.StatusActive,
			Enabled:     true,
		}
		am.Accounts = append(am.Accounts, account)
		log.Printf("[AccountManager] Loaded account: %s (%s) auth=%s", acc.Email, acc.IDP, creds.GetAuthMethod())
	}

	log.Printf("[AccountManager] Loaded %d active accounts", len(am.Accounts))
	return nil
}

func (am *AccountManager) SaveAccounts() error {
	am.Mu.RLock()
	defer am.Mu.RUnlock()

	af, err := kiro.LoadAccountsFromFile(am.accountsFile)
	if err != nil {
		return err
	}

	// Update tokens in the file
	emailMap := make(map[string]*Account)
	for _, acc := range am.Accounts {
		emailMap[acc.Email] = acc
	}

	for i, jsonAcc := range af.Accounts {
		if acc, ok := emailMap[jsonAcc.Email]; ok {
			acc.Mu.Lock()
			if acc.Credentials != nil {
				af.Accounts[i].Credentials = *acc.Credentials
			}
			acc.Mu.Unlock()
		}
	}

	return kiro.SaveAccountsToFile(am.accountsFile, af)
}

// GetNextAccount selects the best available account using session stickiness + weighted scoring.
// Kept for backward compatibility (non-blocking). Calls GetNextAccountWait with background context.
func (am *AccountManager) GetNextAccount(sessionID string, model ...string) *Account {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return am.GetNextAccountWait(ctx, sessionID, model...)
}

// GetNextAccountWait selects an account, blocking until a slot is available or ctx is cancelled.
// Session-bound requests will always wait for their bound account (unless it's banned/disabled).
func (am *AccountManager) GetNextAccountWait(ctx context.Context, sessionID string, model ...string) *Account {
	requestModel := ""
	if len(model) > 0 {
		requestModel = model[0]
	}

	// 查找 session 绑定的账号
	var stickyAcc *Account
	if sessionID != "" {
		var targetAccountID string
		am.sessionsMu.Lock()
		if entry, ok := am.sessions[sessionID]; ok {
			targetAccountID = entry.accountID
		}
		am.sessionsMu.Unlock()

		if targetAccountID != "" {
			am.Mu.RLock()
			for _, a := range am.Accounts {
				if a.ID == targetAccountID {
					stickyAcc = a
					break
				}
			}
			am.Mu.RUnlock()
		}

		// 如果绑定的账号被封禁/禁用/不支持模型，清除绑定
		// cooldown 的账号不清除绑定，等它冷却结束继续用
		if stickyAcc != nil {
			stickyAcc.Mu.Lock()
			isBanned := stickyAcc.Status == kiro.StatusSuspended || stickyAcc.Status == kiro.StatusDisabled || !stickyAcc.Enabled
			stickyAcc.Mu.Unlock()
			if isBanned || !stickyAcc.SupportsModel(requestModel) {
				am.sessionsMu.Lock()
				delete(am.sessions, sessionID)
				am.sessionsMu.Unlock()
				stickyAcc = nil
			}
		}
	}

	// 先尝试一次非阻塞获取
	if stickyAcc != nil {
		if stickyAcc.AcquireSlot() {
			stickyAcc.Mu.Lock()
			stickyAcc.LastUsedAt = time.Now()
			stickyAcc.Mu.Unlock()
			atomic.AddInt64(&stickyAcc.RequestCount, 1)
			am.sessionsMu.Lock()
			am.sessions[sessionID] = &sessionEntry{accountID: stickyAcc.ID, lastUsed: time.Now()}
			am.sessionsMu.Unlock()
			return stickyAcc
		}
	} else {
		acc := am.tryPickAccount(requestModel)
		if acc != nil {
			if sessionID != "" {
				am.sessionsMu.Lock()
				am.sessions[sessionID] = &sessionEntry{accountID: acc.ID, lastUsed: time.Now()}
				am.sessionsMu.Unlock()
			}
			return acc
		}
	}

	// 首次获取失败，进入排队等待前先检查：是否有任何账号支持该模型
	// 如果没有，直接返回 nil，避免无限排队
	if stickyAcc == nil && requestModel != "" && !am.hasAccountForModel(requestModel) {
		log.Printf("[AccountManager] 没有账号支持模型 %s，跳过排队", requestModel)
		return nil
	}

	atomic.AddInt64(&am.QueuedRequests, 1)
	defer atomic.AddInt64(&am.QueuedRequests, -1)

	if stickyAcc != nil {
		am.addAccountQueued(stickyAcc.ID, 1)
		defer am.addAccountQueued(stickyAcc.ID, -1)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-am.slotNotify:
		}

		// 有 session 绑定 → 只等这个账号
		if stickyAcc != nil {
			if stickyAcc.AcquireSlot() {
				stickyAcc.Mu.Lock()
				stickyAcc.LastUsedAt = time.Now()
				stickyAcc.Mu.Unlock()
				atomic.AddInt64(&stickyAcc.RequestCount, 1)
				am.sessionsMu.Lock()
				am.sessions[sessionID] = &sessionEntry{accountID: stickyAcc.ID, lastUsed: time.Now()}
				am.sessionsMu.Unlock()
				return stickyAcc
			}
			// 没抢到，把通知传递给其他等待者
			am.notifySlot()
			continue
		}

		// 无 session 绑定 → 加权评分选号
		acc := am.tryPickAccount(requestModel)
		if acc != nil {
			if sessionID != "" {
				am.sessionsMu.Lock()
				am.sessions[sessionID] = &sessionEntry{accountID: acc.ID, lastUsed: time.Now()}
				am.sessionsMu.Unlock()
			}
			return acc
		}
		// 没抢到，把通知传递给其他等待者
		am.notifySlot()
	}
}

// hasAccountForModel 检查是否有任何已启用的账号支持指定模型
func (am *AccountManager) hasAccountForModel(model string) bool {
	am.Mu.RLock()
	defer am.Mu.RUnlock()
	for _, acc := range am.Accounts {
		if acc.Enabled && acc.SupportsModel(model) {
			return true
		}
	}
	return false
}

// tryPickAccount 尝试用加权评分选一个有空闲 slot 的账号（非阻塞）
func (am *AccountManager) tryPickAccount(requestModel string) *Account {
	am.Mu.RLock()
	defer am.Mu.RUnlock()

	if len(am.Accounts) == 0 {
		return nil
	}

	// 统计每个账号绑定的 session 数量（用于均衡分配）
	sessionCounts := am.GetSessionCountByAccount()

	type candidate struct {
		acc   *Account
		score float64
	}
	var candidates []candidate

	now := time.Now()
	for _, acc := range am.Accounts {
		if !acc.IsAvailable() {
			continue
		}
		if !acc.SupportsModel(requestModel) {
			continue
		}
		score := 100.0

		// 并发负载惩罚 (0-50)
		active := float64(atomic.LoadInt64(&acc.ActiveRequests))
		maxC := float64(acc.GetMaxConcurrent())
		if maxC > 0 {
			score -= (active / maxC) * 50
		}

		// session 数量惩罚：绑定的 session 越多，分数越低 (每个 session -2 分)
		sessionCount := sessionCounts[acc.ID]
		score -= float64(sessionCount) * 2

		acc.Mu.Lock()
		sinceLastUse := now.Sub(acc.LastUsedAt).Seconds()
		recent429 := acc.Recent429Count
		lastSuccess := acc.LastSuccessAt
		acc.Mu.Unlock()

		// 空闲加分 (0-20)
		if sinceLastUse > 20 {
			sinceLastUse = 20
		}
		score += sinceLastUse

		// 429 惩罚
		score -= float64(recent429) * 6

		// 近期成功加分
		if !lastSuccess.IsZero() && now.Sub(lastSuccess) < 5*time.Minute {
			score += 10
		}

		candidates = append(candidates, candidate{acc: acc, score: score})
	}

	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	for _, c := range candidates {
		if c.acc.AcquireSlot() {
			c.acc.Mu.Lock()
			c.acc.LastUsedAt = now
			c.acc.Mu.Unlock()
			atomic.AddInt64(&c.acc.RequestCount, 1)
			return c.acc
		}
	}

	return nil
}

// addAccountQueued 增减某个账号的排队计数
func (am *AccountManager) addAccountQueued(accountID string, delta int64) {
	am.accountQueuedMu.Lock()
	p, ok := am.accountQueued[accountID]
	if !ok {
		var v int64
		p = &v
		am.accountQueued[accountID] = p
	}
	am.accountQueuedMu.Unlock()
	atomic.AddInt64(p, delta)
}

// GetAccountQueued 获取某个账号的排队数
func (am *AccountManager) GetAccountQueued(accountID string) int64 {
	am.accountQueuedMu.RLock()
	p, ok := am.accountQueued[accountID]
	am.accountQueuedMu.RUnlock()
	if !ok {
		return 0
	}
	return atomic.LoadInt64(p)
}

func (am *AccountManager) GetAllAccounts() []*Account {
	am.Mu.RLock()
	defer am.Mu.RUnlock()
	result := make([]*Account, len(am.Accounts))
	copy(result, am.Accounts)
	return result
}

func (am *AccountManager) StartTokenRefreshLoop() {
	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[TokenRefresh] panic in refresh loop: %v, 5秒后重启", r)
					}
				}()
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for range ticker.C {
					am.safeRefreshTokens()
				}
			}()
			// panic 后等待 5 秒重启
			time.Sleep(5 * time.Second)
		}
	}()

	// Also do an immediate check
	go am.safeRefreshTokens()
}

// safeRefreshTokens wraps refreshExpiringTokens with panic recovery
func (am *AccountManager) safeRefreshTokens() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[TokenRefresh] panic in refreshExpiringTokens: %v", r)
		}
	}()
	am.refreshExpiringTokens()
}

func (am *AccountManager) refreshExpiringTokens() {
	am.Mu.RLock()
	accounts := make([]*Account, len(am.Accounts))
	copy(accounts, am.Accounts)
	am.Mu.RUnlock()

	refreshed := 0
	for _, acc := range accounts {
		if acc.Credentials == nil {
			continue
		}
		// Skip suspended accounts — don't waste refresh attempts
		if acc.Status == kiro.StatusSuspended {
			continue
		}

		if acc.Credentials.IsExpiringSoon(15) {
			log.Printf("[TokenRefresh] Token expiring soon for %s, refreshing...", acc.Email)
			var transport http.RoundTripper
			if am.proxyMgr != nil {
				// Use account-bound proxy first, fallback to any
				transport = am.proxyMgr.GetTransportForAccount(acc.ID)
				if transport == nil {
					transport = am.proxyMgr.GetTransport("refresh")
				}
			}
			ok, result := acc.RefreshToken(transport)
			// If proxy failed, retry without proxy
			if !ok && transport != nil {
				log.Printf("[TokenRefresh] 代理刷新失败，回退直连: %s", kiro.TruncStr(result, 100))
				ok, result = acc.RefreshToken()
			}
			if ok {
				refreshed++
				log.Printf("[TokenRefresh] Refreshed token for %s", acc.Email)
				// 刷新成功后也写 DB
				if am.db != nil {
					if err := SaveAccountToDB(am.db, acc); err != nil {
						log.Printf("[TokenRefresh] Failed to save %s to DB: %v", acc.Email, err)
					}
				}
			} else {
				log.Printf("[TokenRefresh] Failed to refresh token for %s: %s", acc.Email, result)
			}
		}
	}

	if refreshed > 0 {
		if err := am.SaveAccounts(); err != nil {
			log.Printf("[TokenRefresh] Failed to save accounts: %v", err)
		} else {
			log.Printf("[TokenRefresh] Saved %d refreshed tokens to file", refreshed)
		}
	}
}

// SetProxyManager sets the proxy manager for token refresh
func (am *AccountManager) SetProxyManager(pm *legacy.ProxyManager) {
	am.proxyMgr = pm
}

// SetDB sets the database for persistence
func (am *AccountManager) SetDB(db *legacy.Database) {
	am.db = db
}

// GetSessionCountByAccount 统计每个账号绑定的 session 数量
func (am *AccountManager) GetSessionCountByAccount() map[string]int {
	am.sessionsMu.Lock()
	defer am.sessionsMu.Unlock()
	counts := make(map[string]int)
	for _, entry := range am.sessions {
		counts[entry.accountID]++
	}
	return counts
}

// CleanStaleSessions removes sessions that haven't been used for over 24 hours
func (am *AccountManager) CleanStaleSessions() {
	am.sessionsMu.Lock()
	defer am.sessionsMu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	cleaned := 0
	for k, v := range am.sessions {
		if v.lastUsed.Before(cutoff) {
			delete(am.sessions, k)
			cleaned++
		}
	}
	if cleaned > 0 {
		log.Printf("[AccountManager] 清理了 %d 个过期会话，剩余 %d 个", cleaned, len(am.sessions))
	}
}

// GetAccountStats returns summary stats for status display
func (am *AccountManager) GetAccountStats() map[string]int {
	am.Mu.RLock()
	defer am.Mu.RUnlock()

	stats := map[string]int{
		"total":     len(am.Accounts),
		"active":    0,
		"suspended": 0,
		"cooldown":  0,
		"disabled":  0,
		"queued":    int(atomic.LoadInt64(&am.QueuedRequests)),
	}
	for _, acc := range am.Accounts {
		acc.Mu.Lock()
		status := acc.Status
		enabled := acc.Enabled
		cooldownUntil := acc.CooldownUntil
		acc.Mu.Unlock()

		switch status {
		case kiro.StatusActive:
			if enabled {
				stats["active"]++
			} else {
				stats["disabled"]++
			}
		case kiro.StatusSuspended:
			stats["suspended"]++
		case kiro.StatusCooldown:
			if time.Now().Before(cooldownUntil) {
				stats["cooldown"]++
			} else {
				stats["active"]++
			}
		case kiro.StatusDisabled:
			stats["disabled"]++
		}
	}
	return stats
}

// ==================== 模型-账号映射管理 ====================

// SetModelAccounts 设置某个模型可用的账号列表
func (am *AccountManager) SetModelAccounts(model string, accountIDs []string) {
	am.modelAccountsMu.Lock()
	defer am.modelAccountsMu.Unlock()
	if len(accountIDs) == 0 {
		delete(am.ModelAccounts, model)
	} else {
		am.ModelAccounts[model] = accountIDs
	}
	// 同步更新每个账号的 SupportedModels
	am.syncAccountModels()
}

// GetModelAccounts 获取某个模型可用的账号列表
func (am *AccountManager) GetModelAccounts(model string) []string {
	am.modelAccountsMu.RLock()
	defer am.modelAccountsMu.RUnlock()
	for k, v := range am.ModelAccounts {
		if strings.EqualFold(k, model) {
			return v
		}
	}
	return nil
}

// GetAllModelAccounts 获取所有模型-账号映射
func (am *AccountManager) GetAllModelAccounts() map[string][]string {
	am.modelAccountsMu.RLock()
	defer am.modelAccountsMu.RUnlock()
	result := make(map[string][]string)
	for k, v := range am.ModelAccounts {
		cp := make([]string, len(v))
		copy(cp, v)
		result[k] = cp
	}
	return result
}

// SyncModelAccounts 公开方法，从 ModelAccounts 同步更新每个账号的 SupportedModels
func (am *AccountManager) SyncModelAccounts() {
	am.modelAccountsMu.RLock()
	defer am.modelAccountsMu.RUnlock()
	am.syncAccountModels()
}

// syncAccountModels 从 ModelAccounts 反向更新每个账号的 SupportedModels
func (am *AccountManager) syncAccountModels() {
	// 构建 accountID -> []model 的反向映射
	accModels := make(map[string][]string)
	for model, accIDs := range am.ModelAccounts {
		for _, id := range accIDs {
			accModels[id] = append(accModels[id], model)
		}
	}
	// 更新每个账号
	for _, acc := range am.Accounts {
		acc.Mu.Lock()
		if models, ok := accModels[acc.ID]; ok {
			acc.SupportedModels = models
		} else if len(am.ModelAccounts) > 0 {
			// 有模型映射配置但该账号不在任何模型下 → 设为空 slice 表示不支持任何模型
			acc.SupportedModels = []string{}
		}
		acc.Mu.Unlock()
	}
}
