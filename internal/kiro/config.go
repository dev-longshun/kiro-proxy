package kiro

import (
	"strings"
	"sync"
)

const (
	KiroAPIURL = "https://q.us-east-1.amazonaws.com/generateAssistantResponse"
	ModelsURL  = "https://q.us-east-1.amazonaws.com/ListAvailableModels"

	DefaultKiroVersion = "0.11.28"
	DefaultNodeVersion = "22.22.0"
	DefaultSDKVersion  = "1.0.34"

	QuotaCooldownSeconds = 300
	MaxTools             = 50
	MaxToolDescLen       = 500

	TokenRefreshInterval = 5 * 60  // 5 minutes
	TokenExpiryBuffer    = 15 * 60 // 15 minutes before expiry

	MaxHistoryItems      = 100 // Max history entries to send
	DefaultMaxConcurrent = 2   // default max concurrent requests per account
)

// modelConfigMu protects ModelStripPatterns, ModelAliases, KiroModels
var modelConfigMu sync.RWMutex

// ModelStripPatterns holds patterns to strip from model names.
var ModelStripPatterns []string

// ModelAliases maps input model names to actual Kiro model names (case-insensitive).
// e.g. "claude-sonnet-4-5" -> "claude-sonnet-4.5"
var ModelAliases = map[string]string{}

// KiroModels is the list of available Kiro models, editable via admin UI.
// Loaded from DB at startup. First one is the default.
var KiroModels = []string{"claude-sonnet-4.5", "claude-sonnet-4", "claude-haiku-4.5"}

// GetKiroModels returns a copy of the current models list (thread-safe)
func GetKiroModels() []string {
	modelConfigMu.RLock()
	defer modelConfigMu.RUnlock()
	cp := make([]string, len(KiroModels))
	copy(cp, KiroModels)
	return cp
}

// DefaultModel returns the first model in KiroModels, or a hardcoded fallback.
func DefaultModel() string {
	modelConfigMu.RLock()
	defer modelConfigMu.RUnlock()
	if len(KiroModels) > 0 {
		return KiroModels[0]
	}
	return "claude-sonnet-4"
}

func MapModelName(model string) string {
	if model == "" {
		return DefaultModel()
	}

	modelConfigMu.RLock()
	stripPatterns := make([]string, len(ModelStripPatterns))
	copy(stripPatterns, ModelStripPatterns)
	aliases := make(map[string]string, len(ModelAliases))
	for k, v := range ModelAliases {
		aliases[k] = v
	}
	modelConfigMu.RUnlock()

	// Step 1: 先清理 — 删除不需要的字符串（如 -20250929, -thinking）
	result := model
	for _, p := range stripPatterns {
		if p != "" {
			lo := strings.ToLower(result)
			lowerP := strings.ToLower(p)
			for strings.Contains(lo, lowerP) {
				idx := strings.Index(lo, lowerP)
				result = result[:idx] + result[idx+len(p):]
				lo = strings.ToLower(result)
			}
		}
	}
	result = strings.Trim(result, "- \t")
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	if result == "" {
		return DefaultModel()
	}
	model = result

	// Step 2: 再映射
	lower := strings.ToLower(model)

	// 统一把 claude-xxx-N-M 格式转成 claude-xxx-N.M 格式
	prefixes := []string{"claude-sonnet-4-", "claude-opus-4-", "claude-haiku-4-"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			suffix := model[len(prefix):]
			base := model[:len(prefix)-1]
			return base + "." + suffix
		}
	}

	// 短名别名
	switch lower {
	case "sonnet-4.5", "sonnet-4-5":
		return "claude-sonnet-4.5"
	case "sonnet-4":
		return "claude-sonnet-4"
	case "haiku-4.5", "haiku-4-5":
		return "claude-haiku-4.5"
	case "opus-4.5", "opus-4-5":
		return "claude-opus-4.5"
	}

	// Check alias mapping (case-insensitive)
	if mapped, ok := aliases[lower]; ok {
		return mapped
	}

	return model
}

func Contains(s, substr string) bool {
	return len(s) >= len(substr) && SearchString(s, substr)
}

func SearchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			c := s[i+j]
			d := substr[j]
			// case insensitive
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			if d >= 'A' && d <= 'Z' {
				d += 32
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
