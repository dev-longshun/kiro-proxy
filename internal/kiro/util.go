package kiro

import "bytes"

// TruncStr truncates a string to maxLen characters, appending "..." if truncated.
func TruncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// CleanNullBytes removes null bytes from a byte slice.
func CleanNullBytes(data []byte) []byte {
	return bytes.ReplaceAll(data, []byte{0}, nil)
}
