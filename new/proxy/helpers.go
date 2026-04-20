package proxy

// estimateTokens 估算 token 数，内容非空时至少返回 1
func estimateTokens(content string) int {
	n := len(content) / 4
	if n == 0 && len(content) > 0 {
		n = 1
	}
	return n
}

func mapStopReason(reason, protocol string) string {
	switch protocol {
	case "openai":
		switch reason {
		case "end_turn":
			return "stop"
		case "tool_use":
			return "tool_calls"
		default:
			return "stop"
		}
	default:
		return reason
	}
}
