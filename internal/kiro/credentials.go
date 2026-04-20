package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type CredentialStatus string

const (
	StatusActive    CredentialStatus = "active"
	StatusCooldown  CredentialStatus = "cooldown"
	StatusUnhealthy CredentialStatus = "unhealthy"
	StatusDisabled  CredentialStatus = "disabled"
	StatusSuspended CredentialStatus = "suspended"
)

type KiroCredentials struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	ProfileArn   string `json:"profileArn,omitempty"`
	ExpiresAt    any    `json:"expiresAt,omitempty"` // can be string(ISO) or number(unix ms)
	Region       string `json:"region"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider,omitempty"`
	LastRefresh  string `json:"lastRefresh,omitempty"`
}

func (c *KiroCredentials) GetRegion() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *KiroCredentials) GetAuthMethod() string {
	if c.AuthMethod == "" {
		return "social"
	}
	return strings.ToLower(c.AuthMethod)
}

func (c *KiroCredentials) IsExpired() bool {
	if c.ExpiresAt == nil {
		return true
	}
	expiresUnix := c.getExpiresAtUnixMs()
	if expiresUnix == 0 {
		return true
	}
	nowMs := time.Now().UnixMilli()
	return nowMs >= (expiresUnix - 5*60*1000) // 5 min buffer
}

func (c *KiroCredentials) IsExpiringSoon(minutes int) bool {
	if c.ExpiresAt == nil {
		return false
	}
	expiresUnix := c.getExpiresAtUnixMs()
	if expiresUnix == 0 {
		return false
	}
	nowMs := time.Now().UnixMilli()
	return nowMs >= (expiresUnix - int64(minutes)*60*1000)
}

func (c *KiroCredentials) getExpiresAtUnixMs() int64 {
	switch v := c.ExpiresAt.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		if strings.Contains(v, "T") {
			t, err := time.Parse(time.RFC3339, strings.Replace(v, "Z", "+00:00", 1))
			if err == nil {
				return t.UnixMilli()
			}
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}

// AccountJSON matches the format in kiro-accounts JSON export file
type AccountJSON struct {
	ID          string          `json:"id"`
	Email       string          `json:"email"`
	UserID      string          `json:"userId"`
	Nickname    string          `json:"nickname"`
	IDP         string          `json:"idp"`
	Credentials KiroCredentials `json:"credentials"`
	Subscription struct {
		Type string `json:"type"`
	} `json:"subscription"`
	Usage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
	} `json:"usage"`
	Status string `json:"status"`
	Tags   []string `json:"tags"`
}

type AccountsFile struct {
	Version    string        `json:"version"`
	ExportedAt int64         `json:"exportedAt"`
	Accounts   []AccountJSON `json:"accounts"`
}

func LoadAccountsFromFile(path string) (*AccountsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read accounts file: %w", err)
	}

	var af AccountsFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("parse accounts file: %w", err)
	}
	return &af, nil
}

func SaveAccountsToFile(path string, af *AccountsFile) error {
	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal accounts: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// KiroUsageLimits stores the account's Kiro usage/credits info
type KiroUsageLimits struct {
	SubscriptionTitle string  `json:"subscription_title"`
	SubscriptionType  string  `json:"subscription_type"`
	UsageLimit        float64 `json:"usage_limit"`
	CurrentUsage      float64 `json:"current_usage"`
	FreeTrialStatus   string  `json:"free_trial_status"`
	FreeTrialUsage    float64 `json:"free_trial_usage"`
	FreeTrialLimit    float64 `json:"free_trial_limit"`
	FreeTrialExpiry   float64 `json:"free_trial_expiry"`
	DaysUntilReset    int     `json:"days_until_reset"`
	QueriedAt         time.Time `json:"queried_at"`
}

// IsSuspensionError checks if an error indicates account ban/suspension
// Only 403 is treated as a definitive ban signal.
func IsSuspensionError(statusCode int, errMsg string) bool {
	return statusCode == 403
}
