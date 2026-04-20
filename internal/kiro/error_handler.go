package kiro

import (
	"fmt"
	"strings"
)

// ErrorType represents the type of Kiro API error
type ErrorType string

const (
	ErrorAccountSuspended   ErrorType = "account_suspended"
	ErrorRateLimited        ErrorType = "rate_limited"
	ErrorContentTooLong     ErrorType = "content_too_long"
	ErrorAuthFailed         ErrorType = "auth_failed"
	ErrorServiceUnavailable ErrorType = "service_unavailable"
	ErrorModelUnavailable   ErrorType = "model_unavailable"
	ErrorUnknown            ErrorType = "unknown"
)

// KiroError represents a classified Kiro API error
type KiroError struct {
	Type                 ErrorType
	StatusCode           int
	Message              string
	UserMessage          string
	ShouldDisableAccount bool
	ShouldSwitchAccount  bool
	ShouldRetry          bool
	CooldownSeconds      int
}

// ClassifyError classifies a Kiro API error based on status code and error text
func ClassifyError(statusCode int, errorText string) *KiroError {
	errorLower := strings.ToLower(errorText)

	if strings.Contains(errorLower, "temporarily_suspended") || strings.Contains(errorLower, "suspended") {
		return &KiroError{
			Type:                 ErrorAccountSuspended,
			StatusCode:           statusCode,
			Message:              errorText,
			UserMessage:          "Account has been suspended",
			ShouldDisableAccount: true,
			ShouldSwitchAccount:  true,
		}
	}

	quotaKeywords := []string{"rate limit", "quota", "too many requests", "throttl", "capacity"}
	if statusCode == 429 {
		return &KiroError{
			Type:                ErrorRateLimited,
			StatusCode:          statusCode,
			Message:             errorText,
			UserMessage:         "Rate limited, account entering cooldown",
			ShouldSwitchAccount: true,
			CooldownSeconds:     300,
		}
	}
	for _, kw := range quotaKeywords {
		if strings.Contains(errorLower, kw) {
			return &KiroError{
				Type:                ErrorRateLimited,
				StatusCode:          statusCode,
				Message:             errorText,
				UserMessage:         "Rate limited, account entering cooldown",
				ShouldSwitchAccount: true,
				CooldownSeconds:     300,
			}
		}
	}

	if strings.Contains(errorLower, "content_length_exceeds_threshold") ||
		(strings.Contains(errorLower, "too long") && (strings.Contains(errorLower, "input") || strings.Contains(errorLower, "content"))) {
		return &KiroError{
			Type:        ErrorContentTooLong,
			StatusCode:  statusCode,
			Message:     errorText,
			UserMessage: "Conversation history too long",
			ShouldRetry: true,
		}
	}

	if statusCode == 401 || strings.Contains(errorLower, "unauthorized") || strings.Contains(errorLower, "invalid token") {
		return &KiroError{
			Type:                ErrorAuthFailed,
			StatusCode:          statusCode,
			Message:             errorText,
			UserMessage:         "Token expired or invalid, please refresh",
			ShouldSwitchAccount: true,
		}
	}

	if strings.Contains(errorLower, "model_temporarily_unavailable") || strings.Contains(errorLower, "unexpectedly high load") {
		return &KiroError{
			Type:        ErrorModelUnavailable,
			StatusCode:  statusCode,
			Message:     errorText,
			UserMessage: "Model temporarily unavailable, please retry later",
			ShouldRetry: true,
		}
	}

	if statusCode == 502 || statusCode == 503 || statusCode == 504 || strings.Contains(errorLower, "service unavailable") {
		return &KiroError{
			Type:        ErrorServiceUnavailable,
			StatusCode:  statusCode,
			Message:     errorText,
			UserMessage: "Service temporarily unavailable",
			ShouldRetry: true,
		}
	}

	return &KiroError{
		Type:        ErrorUnknown,
		StatusCode:  statusCode,
		Message:     errorText,
		UserMessage: fmt.Sprintf("API error (%d)", statusCode),
	}
}

// FormatErrorLog formats an error for logging
func FormatErrorLog(err *KiroError, accountID string) string {
	lines := []string{
		fmt.Sprintf("[%s]", strings.ToUpper(string(err.Type))),
	}
	if accountID != "" {
		lines = append(lines, fmt.Sprintf("  Account: %s", accountID))
	}
	lines = append(lines, fmt.Sprintf("  Status: %d", err.StatusCode))
	lines = append(lines, fmt.Sprintf("  Message: %s", err.UserMessage))
	if err.ShouldDisableAccount {
		lines = append(lines, "  Action: Account disabled")
	} else if err.ShouldSwitchAccount {
		lines = append(lines, "  Action: Switching to another account")
	}
	return strings.Join(lines, "\n")
}

// GetAnthropicErrorType maps KiroError type to Anthropic error type string
func GetAnthropicErrorType(errType ErrorType) string {
	switch errType {
	case ErrorAccountSuspended:
		return "authentication_error"
	case ErrorRateLimited:
		return "rate_limit_error"
	case ErrorContentTooLong:
		return "invalid_request_error"
	case ErrorAuthFailed:
		return "authentication_error"
	case ErrorServiceUnavailable:
		return "api_error"
	case ErrorModelUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
