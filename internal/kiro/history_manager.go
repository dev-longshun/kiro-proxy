package kiro

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// HistoryManager manages conversation history with intelligent compression
type HistoryManager struct {
	maxHistoryItems int
	summaryMaxLen   int
	mu              sync.RWMutex
}

func NewHistoryManager() *HistoryManager {
	return &HistoryManager{
		maxHistoryItems: 80,
		summaryMaxLen:   2000,
	}
}

// CompressHistory intelligently compresses conversation history.
func (hm *HistoryManager) CompressHistory(history []interface{}, modelID string) []interface{} {
	if len(history) <= hm.maxHistoryItems {
		return history
	}

	log.Printf("[HistoryManager] Compressing history: %d items (max=%d)", len(history), hm.maxHistoryItems)

	targetKeep := 32
	if targetKeep > hm.maxHistoryItems/2 {
		targetKeep = hm.maxHistoryItems / 2
	}

	splitIdx := hm.findCleanSplitPoint(history, targetKeep)

	olderHistory := history[:splitIdx]
	recentHistory := history[splitIdx:]

	recentHistory = hm.sanitizeRecentStart(recentHistory)
	recentHistory = hm.sanitizeRecentEnd(recentHistory)

	summary := hm.summarizeHistory(olderHistory)

	var compressed []interface{}

	if summary != "" {
		compressed = append(compressed, map[string]interface{}{
			"userInputMessage": map[string]interface{}{
				"content": fmt.Sprintf("[Conversation Summary - %d earlier messages]\n%s", len(olderHistory), summary),
				"modelId": modelID,
				"origin":  "AI_EDITOR",
			},
		})
		compressed = append(compressed, map[string]interface{}{
			"assistantResponseMessage": map[string]interface{}{
				"content": "I understand the conversation context. I'll continue from where we left off.",
			},
		})
	}

	compressed = append(compressed, recentHistory...)

	for len(compressed) > 0 {
		first, ok := compressed[0].(map[string]interface{})
		if !ok {
			compressed = compressed[1:]
			continue
		}
		if first["userInputMessage"] != nil {
			if um, ok := first["userInputMessage"].(map[string]interface{}); ok {
				if ctx, ok := um["userInputMessageContext"].(map[string]interface{}); ok {
					if _, has := ctx["toolResults"]; has {
						delete(ctx, "toolResults")
						if len(ctx) == 0 {
							delete(um, "userInputMessageContext")
						}
					}
				}
			}
			break
		}
		compressed = compressed[1:]
	}

	if len(compressed) > 0 {
		last, ok := compressed[len(compressed)-1].(map[string]interface{})
		if ok && last["userInputMessage"] != nil {
			compressed = append(compressed, map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": "I understand.",
				},
			})
		}
		if ok && last["assistantResponseMessage"] != nil {
			if am, ok := last["assistantResponseMessage"].(map[string]interface{}); ok {
				delete(am, "toolUses")
			}
		}
	}

	log.Printf("[HistoryManager] Compressed: %d -> %d items (summary=%d chars)", len(history), len(compressed), len(summary))
	return compressed
}

func (hm *HistoryManager) findCleanSplitPoint(history []interface{}, targetKeep int) int {
	idealSplit := len(history) - targetKeep
	if idealSplit < 2 {
		idealSplit = 2
	}

	for offset := 0; offset < len(history)/2; offset++ {
		for _, candidate := range []int{idealSplit + offset, idealSplit - offset} {
			if candidate < 2 || candidate >= len(history) {
				continue
			}
			if hm.isCleanBoundary(history, candidate) {
				return candidate
			}
		}
	}

	return idealSplit
}

func (hm *HistoryManager) isCleanBoundary(history []interface{}, idx int) bool {
	if idx <= 0 || idx >= len(history) {
		return false
	}

	prev, ok := history[idx-1].(map[string]interface{})
	if !ok {
		return false
	}
	if am, ok := prev["assistantResponseMessage"].(map[string]interface{}); ok {
		if _, hasToolUses := am["toolUses"]; hasToolUses {
			return false
		}
	} else {
		return false
	}

	cur, ok := history[idx].(map[string]interface{})
	if !ok {
		return false
	}
	if um, ok := cur["userInputMessage"].(map[string]interface{}); ok {
		if ctx, ok := um["userInputMessageContext"].(map[string]interface{}); ok {
			if _, hasToolResults := ctx["toolResults"]; hasToolResults {
				return false
			}
		}
		return true
	}

	return false
}

func (hm *HistoryManager) sanitizeRecentStart(history []interface{}) []interface{} {
	if len(history) == 0 {
		return history
	}

	first, ok := history[0].(map[string]interface{})
	if !ok {
		return history
	}

	if um, ok := first["userInputMessage"].(map[string]interface{}); ok {
		if ctx, ok := um["userInputMessageContext"].(map[string]interface{}); ok {
			if _, has := ctx["toolResults"]; has {
				delete(ctx, "toolResults")
				if len(ctx) == 0 {
					delete(um, "userInputMessageContext")
				}
				if content, _ := um["content"].(string); content == "Tool results provided." {
					um["content"] = "Continue"
				}
			}
		}
	}

	return history
}

func (hm *HistoryManager) sanitizeRecentEnd(history []interface{}) []interface{} {
	if len(history) == 0 {
		return history
	}

	last, ok := history[len(history)-1].(map[string]interface{})
	if !ok {
		return history
	}

	if am, ok := last["assistantResponseMessage"].(map[string]interface{}); ok {
		if _, has := am["toolUses"]; has {
			delete(am, "toolUses")
			if content, _ := am["content"].(string); content == "" {
				am["content"] = "I understand."
			}
		}
	}

	return history
}

func (hm *HistoryManager) summarizeHistory(history []interface{}) string {
	if len(history) == 0 {
		return ""
	}

	var summaryParts []string
	turnCount := 0
	toolUseCount := 0

	for _, item := range history {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		if userMsg, ok := entry["userInputMessage"].(map[string]interface{}); ok {
			content, _ := userMsg["content"].(string)
			if content == "" || content == "Continue" || content == "Tool results provided." {
				continue
			}
			if strings.HasPrefix(content, "[System]") {
				summaryParts = append(summaryParts, "- [System instruction provided]")
				continue
			}
			turnCount++
			truncated := truncateForSummary(content, 150)
			summaryParts = append(summaryParts, fmt.Sprintf("- User[%d]: %s", turnCount, truncated))

			if ctx, ok := userMsg["userInputMessageContext"].(map[string]interface{}); ok {
				if tr, ok := ctx["toolResults"].([]interface{}); ok {
					toolUseCount += len(tr)
				}
			}
		}

		if assistantMsg, ok := entry["assistantResponseMessage"].(map[string]interface{}); ok {
			content, _ := assistantMsg["content"].(string)
			if content == "" || content == "I understand." {
				continue
			}
			truncated := truncateForSummary(content, 200)
			summaryParts = append(summaryParts, fmt.Sprintf("- Assistant[%d]: %s", turnCount, truncated))

			if toolUses, ok := assistantMsg["toolUses"].([]interface{}); ok {
				for _, tu := range toolUses {
					if tuMap, ok := tu.(map[string]interface{}); ok {
						name, _ := tuMap["name"].(string)
						if name != "" {
							summaryParts = append(summaryParts, fmt.Sprintf("  [Tool: %s]", name))
						}
					}
				}
				toolUseCount += len(toolUses)
			}
		}
	}

	header := fmt.Sprintf("Conversation had %d turns with %d tool interactions.", turnCount, toolUseCount)
	result := header + "\n" + strings.Join(summaryParts, "\n")

	if len(result) > hm.summaryMaxLen {
		result = result[:hm.summaryMaxLen-3] + "..."
	}

	return result
}

// EstimateTokenCount provides a rough token count estimate for history
func (hm *HistoryManager) EstimateTokenCount(history []interface{}) int {
	data, err := json.Marshal(history)
	if err != nil {
		return 0
	}
	return len(data) / 4
}

// ShouldCompress checks if history needs compression
func (hm *HistoryManager) ShouldCompress(history []interface{}) bool {
	if len(history) > hm.maxHistoryItems {
		return true
	}
	estimatedTokens := hm.EstimateTokenCount(history)
	return estimatedTokens > 50000
}

// truncateForSummary truncates text for summary display
func truncateForSummary(text string, maxLen int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", "")
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	text = strings.TrimSpace(text)
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen-3] + "..."
}

// SummarizeForRenewal 为会话续期生成高质量摘要
// 提取所有对话的关键信息：用户问了什么、AI 做了什么、工具调用结果
func (hm *HistoryManager) SummarizeForRenewal(history []interface{}) string {
	if len(history) == 0 {
		return ""
	}

	var parts []string
	maxSummaryLen := 8000 // 摘要最大字符数，给足空间
	currentLen := 0
	turnNum := 0

	for _, item := range history {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		// 提取 user 消息
		if userMsg, ok := itemMap["userInputMessage"].(map[string]interface{}); ok {
			content, _ := userMsg["content"].(string)
			if content != "" {
				// 跳过 system prompt
				if strings.HasPrefix(content, "--- SYSTEM PROMPT ---") {
					continue
				}
				if strings.HasPrefix(content, "[Conversation Summary") {
					continue
				}
				turnNum++
				// 用户消息保留更多内容（500字）
				text := truncateForSummary(content, 500)
				entry := fmt.Sprintf("[用户#%d] %s", turnNum, text)
				if currentLen+len(entry) > maxSummaryLen {
					parts = append(parts, fmt.Sprintf("... (省略了剩余 %d 轮对话)", len(history)/2-turnNum))
					break
				}
				parts = append(parts, entry)
				currentLen += len(entry)
			}

			// 提取 tool_result 内容
			if toolResults, ok := userMsg["toolResults"].([]interface{}); ok {
				for _, tr := range toolResults {
					trMap, ok := tr.(map[string]interface{})
					if !ok {
						continue
					}
					content, _ := trMap["content"].(string)
					if content != "" {
						text := truncateForSummary(content, 300)
						entry := fmt.Sprintf("[工具结果] %s", text)
						if currentLen+len(entry) > maxSummaryLen {
							break
						}
						parts = append(parts, entry)
						currentLen += len(entry)
					}
				}
			}
		}

		// 提取 assistant 消息
		if assistMsg, ok := itemMap["assistantResponseMessage"].(map[string]interface{}); ok {
			content, _ := assistMsg["content"].(string)
			if content != "" {
				// AI 消息保留更多内容（600字）
				text := truncateForSummary(content, 600)
				entry := fmt.Sprintf("[AI#%d] %s", turnNum, text)
				if currentLen+len(entry) > maxSummaryLen {
					break
				}
				parts = append(parts, entry)
				currentLen += len(entry)
			}

			// 提取 tool_use 信息（包含工具名和参数摘要）
			if toolUses, ok := assistMsg["toolUses"].([]interface{}); ok {
				for _, tu := range toolUses {
					tuMap, ok := tu.(map[string]interface{})
					if !ok {
						continue
					}
					name, _ := tuMap["name"].(string)
					input, _ := tuMap["input"].(string)
					if name != "" {
						inputPreview := truncateForSummary(input, 100)
						entry := fmt.Sprintf("[工具调用] %s", name)
						if inputPreview != "" {
							entry += fmt.Sprintf(" → %s", inputPreview)
						}
						if currentLen+len(entry) > maxSummaryLen {
							break
						}
						parts = append(parts, entry)
						currentLen += len(entry)
					}
				}
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}

	summary := fmt.Sprintf("以下是之前 %d 轮对话的摘要，请基于这些上下文继续对话：\n\n", turnNum) + strings.Join(parts, "\n")

	// 保存摘要到日志文件
	saveSummaryLog(summary, len(history), turnNum)

	return summary
}

// saveSummaryLog 保存摘要到日志文件
func saveSummaryLog(summary string, historyLen, turnNum int) {
	os.MkdirAll("logs", 0755)
	filename := fmt.Sprintf("logs/%s_summary_renewal.log", time.Now().Format("20060102_150405"))
	content := fmt.Sprintf("=== 会话续期摘要 ===\n时间: %s\n原始历史条数: %d\n对话轮数: %d\n摘要长度: %d 字符\n\n%s\n",
		time.Now().Format("2006-01-02 15:04:05"), historyLen, turnNum, len(summary), summary)
	os.WriteFile(filename, []byte(content), 0644)
	log.Printf("[Summary] 摘要已保存到 %s (%d 字符, %d 轮对话)", filename, len(summary), turnNum)
}
