package convert

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"kiro-proxy/internal/kiro"
)

// FixHistoryAlternation ensures history strictly alternates user/assistant messages
func FixHistoryAlternation(history []interface{}, modelID string) []interface{} {
	if len(history) == 0 {
		return history
	}

	var fixed []interface{}

	for _, item := range history {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		isUser := entry["userInputMessage"] != nil
		isAssistant := entry["assistantResponseMessage"] != nil

		if !isUser && !isAssistant {
			continue
		}

		if len(fixed) == 0 {
			if isUser {
				fixed = append(fixed, entry)
			} else {
				fixed = append(fixed, map[string]interface{}{
					"userInputMessage": map[string]interface{}{
						"content": "Continue",
						"modelId": modelID,
						"origin":  "AI_EDITOR",
					},
				})
				fixed = append(fixed, entry)
			}
			continue
		}

		lastEntry := fixed[len(fixed)-1].(map[string]interface{})
		lastIsUser := lastEntry["userInputMessage"] != nil

		if isUser && lastIsUser {
			fixed = append(fixed, map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": "I understand.",
				},
			})
			fixed = append(fixed, entry)
		} else if isAssistant && !lastIsUser {
			prevAssistant := lastEntry["assistantResponseMessage"].(map[string]interface{})
			curAssistant := entry["assistantResponseMessage"].(map[string]interface{})
			prevContent, _ := prevAssistant["content"].(string)
			curContent, _ := curAssistant["content"].(string)
			if curContent != "" {
				if prevContent != "" {
					prevContent += "\n" + curContent
				} else {
					prevContent = curContent
				}
				prevAssistant["content"] = prevContent
			}
			if curToolUses, ok := curAssistant["toolUses"]; ok {
				if curTU, ok := curToolUses.([]interface{}); ok {
					if prevToolUses, ok := prevAssistant["toolUses"].([]interface{}); ok {
						prevAssistant["toolUses"] = append(prevToolUses, curTU...)
					} else {
						prevAssistant["toolUses"] = curTU
					}
				}
			}
		} else {
			fixed = append(fixed, entry)
		}
	}

	if len(fixed) > 0 {
		lastEntry := fixed[len(fixed)-1].(map[string]interface{})
		if lastEntry["userInputMessage"] != nil {
			fixed = append(fixed, map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": "I understand.",
				},
			})
		}
	}

	log.Printf("[HistoryFix] %d -> %d entries", len(history), len(fixed))
	return fixed
}

// GenerateSessionID creates a session hash from first 3 messages
func GenerateSessionID(messages []interface{}) string {
	// 用第一条 user message 的 content 作为 anchor，同一对话的 sessionID 是稳定的
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if m["role"] == "user" {
			content := ""
			switch c := m["content"].(type) {
			case string:
				content = c
			case []interface{}:
				for _, block := range c {
					if b, ok := block.(map[string]interface{}); ok {
						if t, ok := b["text"].(string); ok {
							content = t
							break
						}
					}
				}
			}
			if content != "" {
				h := sha256.Sum256([]byte(content))
				return fmt.Sprintf("%x", h[:8])
			}
		}
	}
	// fallback: 随机 ID
	data, _ := json.Marshal(messages)
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}

// minifySchemaValue 精简 JSON schema，删除 description/default/examples 等字段
// 大幅减小请求体大小，避免 Kiro 看到过多噪音
func minifySchemaValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			switch k {
			case "description", "default", "examples", "title", "$schema":
				continue
			}
			out[k] = minifySchemaValue(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, child := range t {
			out = append(out, minifySchemaValue(child))
		}
		return out
	default:
		return v
	}
}

// ConvertAnthropicToolsToKiro converts Anthropic tool format to Kiro format
func ConvertAnthropicToolsToKiro(tools []interface{}) []interface{} {
	var kiroTools []interface{}
	funcCount := 0

	for _, t := range tools {
		tool, ok := t.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := tool["name"].(string)

		if name == "web_search" || name == "web_search_20250305" {
			kiroTools = append(kiroTools, map[string]interface{}{
				"webSearchTool": map[string]interface{}{
					"type": "web_search",
				},
			})
			continue
		}

		if funcCount >= kiro.MaxTools {
			continue
		}
		funcCount++

		desc := name // 用 tool name 作为 description（不能为空）

		inputSchema := tool["input_schema"]
		if inputSchema == nil {
			inputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		// 精简 schema：删除 description/default/examples 等字段
		inputSchema = minifySchemaValue(inputSchema)

		kiroTools = append(kiroTools, map[string]interface{}{
			"toolSpecification": map[string]interface{}{
				"name":        name,
				"description": desc,
				"inputSchema": map[string]interface{}{
					"json": inputSchema,
				},
			},
		})
	}

	return kiroTools
}

// ConvertOpenAIToolsToKiro converts OpenAI tool format to Kiro format
func ConvertOpenAIToolsToKiro(tools []interface{}) []interface{} {
	var kiroTools []interface{}
	funcCount := 0

	for _, t := range tools {
		tool, ok := t.(map[string]interface{})
		if !ok {
			continue
		}

		toolType, _ := tool["type"].(string)
		if toolType != "function" {
			continue
		}

		fn, ok := tool["function"].(map[string]interface{})
		if !ok {
			continue
		}

		if funcCount >= kiro.MaxTools {
			continue
		}
		funcCount++

		name, _ := fn["name"].(string)
		desc := name // 用 tool name 作为 description（不能为空）

		params := fn["parameters"]
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		params = minifySchemaValue(params)

		kiroTools = append(kiroTools, map[string]interface{}{
			"toolSpecification": map[string]interface{}{
				"name":        name,
				"description": desc,
				"inputSchema": map[string]interface{}{
					"json": params,
				},
			},
		})
	}

	return kiroTools
}

// ExtractImagesFromContent extracts text and images from multimodal content
func ExtractImagesFromContent(content interface{}) (string, []interface{}) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []interface{}:
		var textParts []string
		var images []interface{}

		for _, block := range v {
			b, ok := block.(map[string]interface{})
			if !ok {
				if s, ok := block.(string); ok {
					textParts = append(textParts, s)
				}
				continue
			}

			blockType, _ := b["type"].(string)

			switch blockType {
			case "text":
				text, _ := b["text"].(string)
				textParts = append(textParts, text)

			case "image":
				source, ok := b["source"].(map[string]interface{})
				if !ok {
					continue
				}
				mediaType, _ := source["media_type"].(string)
				data, _ := source["data"].(string)

				imgFmt := "jpeg"
				if strings.Contains(mediaType, "png") {
					imgFmt = "png"
				} else if strings.Contains(mediaType, "gif") {
					imgFmt = "gif"
				} else if strings.Contains(mediaType, "webp") {
					imgFmt = "webp"
				}

				if data != "" {
					images = append(images, map[string]interface{}{
						"format": imgFmt,
						"source": map[string]interface{}{"bytes": data},
					})
				}

			case "image_url":
				imageURL, ok := b["image_url"].(map[string]interface{})
				if !ok {
					continue
				}
				url, _ := imageURL["url"].(string)
				if strings.HasPrefix(url, "data:") {
					re := regexp.MustCompile(`data:image/(\w+);base64,(.+)`)
					matches := re.FindStringSubmatch(url)
					if len(matches) == 3 {
						images = append(images, map[string]interface{}{
							"format": matches[1],
							"source": map[string]interface{}{"bytes": matches[2]},
						})
					}
				}
			}
		}

		return strings.Join(textParts, "\n"), images
	}

	if content != nil {
		return fmt.Sprintf("%v", content), nil
	}
	return "", nil
}

// extractToolResults extracts tool_result blocks from Anthropic content
func extractToolResults(content interface{}) []interface{} {
	contentArr, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var results []interface{}
	for _, block := range contentArr {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		if b["type"] == "tool_result" {
			toolUseID, _ := b["tool_use_id"].(string)
			resultContent, _ := b["content"].(string)
			if resultContent == "" {
				if rc, ok := b["content"].([]interface{}); ok {
					for _, rBlock := range rc {
						if rb, ok := rBlock.(map[string]interface{}); ok {
							if t, ok := rb["text"].(string); ok {
								resultContent = t
							}
						}
					}
				}
			}
			if toolUseID != "" {
				results = append(results, map[string]interface{}{
					"toolUseId": toolUseID,
					"status":    "success",
					"content":   []interface{}{map[string]interface{}{"text": resultContent}},
				})
			}
		}
	}
	return results
}

// ConvertAnthropicMessagesToKiro converts Anthropic message format to Kiro history + current message
func ConvertAnthropicMessagesToKiro(messages []interface{}, tools []interface{}) (string, []interface{}, []interface{}, []interface{}, []interface{}) {
	var history []interface{}
	var kiroTools []interface{}
	var allImages []interface{}
	var currentToolResults []interface{}
	userContent := ""

	if len(tools) > 0 {
		kiroTools = ConvertAnthropicToolsToKiro(tools)
	}

	lastAssistantHadToolUses := false

	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := m["role"].(string)
		content := m["content"]

		if role == "user" {
			text, imgs := ExtractImagesFromContent(content)
			if len(imgs) > 0 {
				allImages = append(allImages, imgs...)
			}

			toolResults := extractToolResults(content)

			if i == len(messages)-1 {
				userContent = text
				if len(toolResults) > 0 {
					currentToolResults = toolResults
				}
			} else {
				userMsg := map[string]interface{}{
					"content": text,
					"modelId": "claude-sonnet-4",
					"origin":  "AI_EDITOR",
				}

				if lastAssistantHadToolUses && len(toolResults) > 0 {
					userMsg["userInputMessageContext"] = map[string]interface{}{
						"toolResults": toolResults,
					}
				}

				if text != "" || len(toolResults) > 0 {
					if text == "" {
						userMsg["content"] = "Tool results provided."
					}
					history = append(history, map[string]interface{}{
						"userInputMessage": userMsg,
					})
				}
			}
			lastAssistantHadToolUses = false

		} else if role == "assistant" {
			text, _ := ExtractImagesFromContent(content)

			var toolUses []interface{}
			if contentArr, ok := content.([]interface{}); ok {
				for _, block := range contentArr {
					b, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					if b["type"] == "tool_use" {
						toolUses = append(toolUses, map[string]interface{}{
							"toolUseId": b["id"],
							"name":      b["name"],
							"input":     b["input"],
						})
					}
				}
			}

			assistantText := text
			if assistantText == "" {
				assistantText = "I understand."
			}

			histEntry := map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": assistantText,
				},
			}

			if len(toolUses) > 0 {
				histEntry["assistantResponseMessage"].(map[string]interface{})["toolUses"] = toolUses
				lastAssistantHadToolUses = true
			} else {
				lastAssistantHadToolUses = false
			}

			history = append(history, histEntry)
		}
	}

	if userContent == "" {
		userContent = "Continue"
	}

	return userContent, history, kiroTools, allImages, currentToolResults
}

// ConvertOpenAIMessagesToKiro converts OpenAI message format to Kiro format
func ConvertOpenAIMessagesToKiro(messages []interface{}, tools []interface{}) (string, []interface{}, []interface{}, []interface{}, []interface{}) {
	var history []interface{}
	var kiroTools []interface{}
	var allImages []interface{}
	var currentToolResults []interface{}
	userContent := ""
	systemPrompt := ""

	if len(tools) > 0 {
		kiroTools = ConvertOpenAIToolsToKiro(tools)
	}

	var pendingToolResults []interface{}

	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := m["role"].(string)
		content := m["content"]

		switch role {
		case "system":
			flushToolResults(&history, &pendingToolResults)

			text, _ := content.(string)
			if text != "" {
				// 保存 system prompt，后面拼到第一条 user message 前面
				if systemPrompt == "" {
					systemPrompt = text
				} else {
					systemPrompt += "\n" + text
				}
			}

		case "user":
			flushToolResults(&history, &pendingToolResults)

			text, imgs := ExtractImagesFromContent(content)
			if len(imgs) > 0 {
				allImages = append(allImages, imgs...)
			}

			if i == len(messages)-1 {
				userContent = text
			} else {
				history = append(history, map[string]interface{}{
					"userInputMessage": map[string]interface{}{
						"content": text,
						"modelId": "claude-sonnet-4",
						"origin":  "AI_EDITOR",
					},
				})
			}

		case "assistant":
			flushToolResults(&history, &pendingToolResults)

			text, _ := content.(string)
			histEntry := map[string]interface{}{
				"assistantResponseMessage": map[string]interface{}{
					"content": text,
				},
			}

			if toolCalls, ok := m["tool_calls"].([]interface{}); ok {
				var toolUses []interface{}
				for _, tc := range toolCalls {
					call, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := call["function"].(map[string]interface{})
					if fn == nil {
						continue
					}

					args := fn["arguments"]
					var inputJSON interface{}
					if argsStr, ok := args.(string); ok {
						json.Unmarshal([]byte(argsStr), &inputJSON)
					} else {
						inputJSON = args
					}

					toolUses = append(toolUses, map[string]interface{}{
						"toolUseId": call["id"],
						"name":      fn["name"],
						"input":     inputJSON,
					})
				}
				if len(toolUses) > 0 {
					histEntry["assistantResponseMessage"].(map[string]interface{})["toolUses"] = toolUses
				}
			}

			history = append(history, histEntry)

		case "tool":
			toolCallID, _ := m["tool_call_id"].(string)
			resultContent, _ := content.(string)
			pendingToolResults = append(pendingToolResults, map[string]interface{}{
				"toolUseId": toolCallID,
				"status":    "success",
				"content":   []interface{}{map[string]interface{}{"text": resultContent}},
			})
		}
	}

	if len(pendingToolResults) > 0 {
		currentToolResults = pendingToolResults
	}

	if userContent == "" {
		userContent = "Continue"
	}

	// system prompt 只在第一次请求（无历史）时注入
	// 后续请求 Kiro 服务端已经缓存了上下文
	if systemPrompt != "" && len(history) == 0 {
		userContent = systemPrompt + "\n\n" + userContent
	}

	return userContent, history, kiroTools, allImages, currentToolResults
}

// flushToolResults adds pending tool results as a user message in history
func flushToolResults(history *[]interface{}, pending *[]interface{}) {
	if len(*pending) == 0 {
		return
	}
	*history = append(*history, map[string]interface{}{
		"userInputMessage": map[string]interface{}{
			"content": "Tool results provided.",
			"modelId": "claude-sonnet-4",
			"origin":  "AI_EDITOR",
			"userInputMessageContext": map[string]interface{}{
				"toolResults": *pending,
			},
		},
	})
	*pending = nil
}
