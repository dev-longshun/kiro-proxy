package legacy

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// RateLimitConfig holds rate limiting configuration
type RateLimitConfig struct {
	Enabled                    bool
	MinRequestInterval         float64
	MaxRequestsPerMinute       int
	GlobalMaxRequestsPerMinute int
	QuotaCooldownSeconds       int

	// Retry settings
	RetryTimeoutSeconds    int     // max retry duration per request (default 60)
	Retry429DelaySeconds   float64 // delay after 429 before retry (default 0 = immediate)
	RetryMaxAttempts       int     // max retry attempts for same session+account (default 100)
	RetryErrorDelaySeconds float64 // delay between retries on other errors

	// Account cooldown settings
	CooldownThreshold int // consecutive non-429 errors before cooldown

	// Connection settings
	ConnectTimeoutSeconds int // TCP+TLS connect timeout (default 15)
}

// AccountRateState tracks per-account rate state
type AccountRateState struct {
	LastRequestTime time.Time
	RequestTimes    []time.Time
}

func (s *AccountRateState) GetRequestsInWindow(windowSeconds int) int {
	cutoff := time.Now().Add(-time.Duration(windowSeconds) * time.Second)
	count := 0
	for _, t := range s.RequestTimes {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

// RateLimiter controls request rates
type RateLimiter struct {
	Config        RateLimitConfig
	accountStates map[string]*AccountRateState
	globalTimes   []time.Time
	mu            sync.Mutex
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		Config: RateLimitConfig{
			Enabled:                    false,
			MinRequestInterval:         0.5,
			MaxRequestsPerMinute:       60,
			GlobalMaxRequestsPerMinute: 120,
			QuotaCooldownSeconds:       30,
			RetryTimeoutSeconds:        60,
			Retry429DelaySeconds:       0,
			RetryMaxAttempts:           100,
			RetryErrorDelaySeconds:     1.0,
			CooldownThreshold:          10,
			ConnectTimeoutSeconds:      15,
		},
		accountStates: make(map[string]*AccountRateState),
	}
}

func (rl *RateLimiter) getAccountState(accountID string) *AccountRateState {
	if _, ok := rl.accountStates[accountID]; !ok {
		rl.accountStates[accountID] = &AccountRateState{}
	}
	return rl.accountStates[accountID]
}

// CanRequest checks if a request can be made
func (rl *RateLimiter) CanRequest(accountID string) (bool, float64, string) {
	if !rl.Config.Enabled {
		return true, 0, ""
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	state := rl.getAccountState(accountID)

	// Check min interval
	if !state.LastRequestTime.IsZero() {
		elapsed := now.Sub(state.LastRequestTime).Seconds()
		if elapsed < rl.Config.MinRequestInterval {
			wait := rl.Config.MinRequestInterval - elapsed
			return false, wait, "Request too fast"
		}
	}

	// Check per-account RPM
	accountRPM := state.GetRequestsInWindow(60)
	if accountRPM >= rl.Config.MaxRequestsPerMinute {
		cooldown := float64(rl.Config.QuotaCooldownSeconds)
		if cooldown <= 0 {
			cooldown = 2
		}
		return false, cooldown, "Account requests too frequent"
	}

	// Check global RPM
	cutoff := now.Add(-60 * time.Second)
	globalRPM := 0
	for _, t := range rl.globalTimes {
		if t.After(cutoff) {
			globalRPM++
		}
	}
	if globalRPM >= rl.Config.GlobalMaxRequestsPerMinute {
		cooldown := float64(rl.Config.QuotaCooldownSeconds)
		if cooldown <= 0 {
			cooldown = 1
		}
		return false, cooldown, "Global requests too frequent"
	}

	return true, 0, ""
}

// RecordRequest records a request for rate limiting
func (rl *RateLimiter) RecordRequest(accountID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	state := rl.getAccountState(accountID)
	state.LastRequestTime = now
	state.RequestTimes = append(state.RequestTimes, now)

	// Keep only last 100 entries
	if len(state.RequestTimes) > 100 {
		state.RequestTimes = state.RequestTimes[len(state.RequestTimes)-100:]
	}

	rl.globalTimes = append(rl.globalTimes, now)
	if len(rl.globalTimes) > 1000 {
		rl.globalTimes = rl.globalTimes[len(rl.globalTimes)-1000:]
	}

	// 清理超过 1 小时没有请求的 accountStates
	if len(rl.accountStates) > 50 {
		cutoff := now.Add(-1 * time.Hour)
		for id, s := range rl.accountStates {
			if s.LastRequestTime.Before(cutoff) {
				delete(rl.accountStates, id)
			}
		}
	}
}

// GetStats returns rate limiter statistics
func (rl *RateLimiter) GetStats() map[string]interface{} {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	globalRPM := 0
	for _, t := range rl.globalTimes {
		if t.After(cutoff) {
			globalRPM++
		}
	}

	accounts := map[string]interface{}{}
	for aid, state := range rl.accountStates {
		accounts[aid] = map[string]interface{}{
			"rpm": state.GetRequestsInWindow(60),
		}
	}

	return map[string]interface{}{
		"enabled":    rl.Config.Enabled,
		"global_rpm": globalRPM,
		"config": map[string]interface{}{
			"enabled":                        rl.Config.Enabled,
			"min_request_interval":           rl.Config.MinRequestInterval,
			"max_requests_per_minute":        rl.Config.MaxRequestsPerMinute,
			"global_max_requests_per_minute": rl.Config.GlobalMaxRequestsPerMinute,
			"quota_cooldown_seconds":         rl.Config.QuotaCooldownSeconds,
		},
		"accounts": accounts,
	}
}

// UpdateConfig updates the rate limiter configuration thread-safely
func (rl *RateLimiter) UpdateConfig(cfg RateLimitConfig) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.Config = cfg
}

// LoadConfigFromDB loads rate limit config from database settings
func (rl *RateLimiter) LoadConfigFromDB(db *Database) {
	if db == nil {
		return
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if v := db.GetSetting("ratelimit_enabled"); v != "" {
		rl.Config.Enabled = v == "true"
	}
	if v := db.GetSetting("ratelimit_min_interval"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f >= 0 {
			rl.Config.MinRequestInterval = f
		}
	}
	if v := db.GetSetting("ratelimit_max_rpm"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			rl.Config.MaxRequestsPerMinute = n
		}
	}
	if v := db.GetSetting("ratelimit_global_max_rpm"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			rl.Config.GlobalMaxRequestsPerMinute = n
		}
	}
	if v := db.GetSetting("ratelimit_cooldown"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			rl.Config.QuotaCooldownSeconds = n
		}
	}
	if v := db.GetSetting("retry_timeout_seconds"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			rl.Config.RetryTimeoutSeconds = n
		}
	}
	if v := db.GetSetting("retry_429_delay"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f >= 0 {
			rl.Config.Retry429DelaySeconds = f
		}
	}
	if v := db.GetSetting("retry_max_attempts"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			rl.Config.RetryMaxAttempts = n
		}
	}
	if v := db.GetSetting("retry_error_delay"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f >= 0 {
			rl.Config.RetryErrorDelaySeconds = f
		}
	}
	if v := db.GetSetting("cooldown_threshold"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			rl.Config.CooldownThreshold = n
		}
	}
	if v := db.GetSetting("connect_timeout_seconds"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			rl.Config.ConnectTimeoutSeconds = n
		}
	}

	log.Printf("[RateLimiter] 从数据库加载配置: enabled=%v interval=%.1fs rpm=%d globalRpm=%d cooldown=%ds retryTimeout=%ds 429delay=%.1fs maxAttempts=%d errDelay=%.1fs cdThreshold=%d connectTimeout=%ds",
		rl.Config.Enabled, rl.Config.MinRequestInterval,
		rl.Config.MaxRequestsPerMinute, rl.Config.GlobalMaxRequestsPerMinute,
		rl.Config.QuotaCooldownSeconds, rl.Config.RetryTimeoutSeconds,
		rl.Config.Retry429DelaySeconds, rl.Config.RetryMaxAttempts, rl.Config.RetryErrorDelaySeconds,
		rl.Config.CooldownThreshold, rl.Config.ConnectTimeoutSeconds)
}

// SaveConfigToDB saves rate limit config to database settings
func (rl *RateLimiter) SaveConfigToDB(db *Database) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}

	rl.mu.Lock()
	cfg := rl.Config
	rl.mu.Unlock()

	if err := db.SetSetting("ratelimit_enabled", fmt.Sprintf("%v", cfg.Enabled)); err != nil {
		return err
	}
	if err := db.SetSetting("ratelimit_min_interval", fmt.Sprintf("%.2f", cfg.MinRequestInterval)); err != nil {
		return err
	}
	if err := db.SetSetting("ratelimit_max_rpm", fmt.Sprintf("%d", cfg.MaxRequestsPerMinute)); err != nil {
		return err
	}
	if err := db.SetSetting("ratelimit_global_max_rpm", fmt.Sprintf("%d", cfg.GlobalMaxRequestsPerMinute)); err != nil {
		return err
	}
	if err := db.SetSetting("ratelimit_cooldown", fmt.Sprintf("%d", cfg.QuotaCooldownSeconds)); err != nil {
		return err
	}
	if err := db.SetSetting("retry_timeout_seconds", fmt.Sprintf("%d", cfg.RetryTimeoutSeconds)); err != nil {
		return err
	}
	if err := db.SetSetting("retry_429_delay", fmt.Sprintf("%.2f", cfg.Retry429DelaySeconds)); err != nil {
		return err
	}
	if err := db.SetSetting("retry_max_attempts", fmt.Sprintf("%d", cfg.RetryMaxAttempts)); err != nil {
		return err
	}
	if err := db.SetSetting("retry_error_delay", fmt.Sprintf("%.2f", cfg.RetryErrorDelaySeconds)); err != nil {
		return err
	}
	if err := db.SetSetting("cooldown_threshold", fmt.Sprintf("%d", cfg.CooldownThreshold)); err != nil {
		return err
	}
	if err := db.SetSetting("connect_timeout_seconds", fmt.Sprintf("%d", cfg.ConnectTimeoutSeconds)); err != nil {
		return err
	}
	return nil
}

// GetConfig returns a copy of the current config (thread-safe)
func (rl *RateLimiter) GetConfig() RateLimitConfig {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.Config
}

// UpdateConfigFields updates individual config fields (thread-safe)
func (rl *RateLimiter) UpdateConfigFields(updater func(cfg *RateLimitConfig)) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	updater(&rl.Config)
}

// GlobalRateLimiter is the global rate limiter instance
var GlobalRateLimiter = NewRateLimiter()
