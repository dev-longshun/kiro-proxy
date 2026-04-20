package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionTracker 追踪每个会话的上下文使用率
type SessionTracker struct {
	mu         sync.RWMutex
	sessions   map[string]*SessionInfo
	cacheStore CacheStore // 缓存存储（Redis 或内存）
}

type SessionInfo struct {
	ContextUsage float64   // Kiro 返回的 contextUsagePercentage
	LastUpdate   time.Time
	RequestCount int
	UserID       string // 用户标识
	Source       string // 来源: openclaw, claude-cli, unknown
	Summary      string // 上下文摘要（用于新会话时携带记忆）
	Renewed      bool   // 是否已经续期过（开了新会话）
}

// SPLICE_1

func NewSessionTracker() *SessionTracker {
	st := &SessionTracker{
		sessions:   make(map[string]*SessionInfo),
		cacheStore: NewMemoryCacheStore(), // 默认内存，可通过 SetCacheStore 切换 Redis
	}
	return st
}

// SetCacheStore 设置缓存存储后端
func (st *SessionTracker) SetCacheStore(store CacheStore) {
	st.cacheStore = store
}

func (st *SessionTracker) cleanup() {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	cleaned := 0
	for k, v := range st.sessions {
		if v.LastUpdate.Before(cutoff) {
			delete(st.sessions, k)
			cleaned++
		}
	}
	if cleaned > 0 {
		log.Printf("[SessionTracker] 清理了 %d 个过期会话，剩余 %d 个", cleaned, len(st.sessions))
	}
}

// UpdateContext 更新会话的上下文使用率
func (st *SessionTracker) UpdateContext(sessionKey string, ctxPercent float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if info, ok := st.sessions[sessionKey]; ok {
		info.ContextUsage = ctxPercent
		info.LastUpdate = time.Now()
		info.RequestCount++
	} else {
		st.sessions[sessionKey] = &SessionInfo{
			ContextUsage: ctxPercent,
			LastUpdate:   time.Now(),
			RequestCount: 1,
		}
	}
}

// CacheHashes 缓存计算结果
type CacheHashes struct {
	BaseHash   string // system + tools 的 hash
	BaseTokens int    // system + tools 的估算 tokens
}

// CacheResult 缓存命中结果
type CacheResult struct {
	ReadTokens     int // 从缓存读取的 token 数 (cache_read_input_tokens)
	CreationTokens int // 写入缓存的 token 数 (cache_creation_input_tokens)
}

// MessageHash 单条 message 的 hash 和 token 估算
type MessageHash struct {
	Hash   string
	Tokens int
}

// ComputeMessageHashes 计算每条 message 的 hash 和 token 数
// 用 role + content text 的 sha256 前 16 位作为 key，不管 text 里有什么特殊字符
func ComputeMessageHashes(messages []interface{}) []MessageHash {
	result := make([]MessageHash, 0, len(messages))
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		// 序列化整条 message 算 hash（包含 role + content，确保唯一性）
		msgBytes, _ := json.Marshal(m)
		h := sha256.Sum256(msgBytes)
		hash := fmt.Sprintf("%x", h[:8])
		tokens := estimateTokens(string(msgBytes))
		if tokens < 1 {
			tokens = 1
		}
		result = append(result, MessageHash{Hash: hash, Tokens: tokens})
	}
	return result
}

// GetCachedTokensPerMessage 逐条 message 匹配缓存
//
// 缓存规则：
//   - 只有 messages 数组里的历史消息参与缓存（最后一条是新消息，不参与）
//   - 历史消息命中缓存 → cache_read（0.1x 价格）
//   - 历史消息未命中 → cache_creation（写入缓存，1.25x 价格）
//   - 最后一条新消息 → 普通 input（不参与缓存计算）
//   - system、tools 等非 messages 字段 → 始终算普通 input（baseTokens 不参与缓存）
//
// input_tokens = baseTokens + 最后一条 msg tokens + (未命中也未写入的部分，通常为 0)
// cache_read = 命中缓存的历史 msg tokens
// cache_creation = 未命中缓存的历史 msg tokens
// 三者加起来 = 总输入 tokens
func (st *SessionTracker) GetCachedTokensPerMessage(msgHashes []MessageHash, hashes CacheHashes) CacheResult {
	readTokens := 0
	creationTokens := 0

	// messages 里除最后一条外都是历史，参与缓存匹配
	if len(msgHashes) > 1 {
		historyMsgs := msgHashes[:len(msgHashes)-1]
		for _, mh := range historyMsgs {
			if _, ok := st.cacheStore.Get("msg:" + mh.Hash); ok {
				readTokens += mh.Tokens
			} else {
				creationTokens += mh.Tokens
			}
		}
	}
	// 最后一条（新消息）和 base（system+tools）都算普通 input，不在这里累加

	return CacheResult{ReadTokens: readTokens, CreationTokens: creationTokens}
}

// UpdateCachePerMessage 请求成功后，把所有 messages 的 hash 写入缓存
// 下次相同 message 就能命中 cache_read
func (st *SessionTracker) UpdateCachePerMessage(msgHashes []MessageHash, hashes CacheHashes) {
	for _, mh := range msgHashes {
		st.cacheStore.Set("msg:"+mh.Hash, mh.Tokens)
	}
}

// ComputeContentHash 计算请求内容的 base hash（system + tools）
func ComputeContentHash(body map[string]interface{}) CacheHashes {
	h := sha256.New()

	// 1. system prompt
	if sys, ok := body["system"]; ok {
		sysBytes, _ := json.Marshal(sys)
		h.Write(sysBytes)
	}

	// 2. tools
	if tools, ok := body["tools"]; ok {
		toolsBytes, _ := json.Marshal(tools)
		h.Write(toolsBytes)
	}

	baseHash := fmt.Sprintf("%x", h.Sum(nil))[:16]

	// 估算 base tokens（system + tools 的 JSON 大小）
	baseBytes, _ := json.Marshal(map[string]interface{}{
		"system": body["system"],
		"tools":  body["tools"],
	})
	baseTokens := estimateTokens(string(baseBytes))

	return CacheHashes{
		BaseHash:   baseHash,
		BaseTokens: baseTokens,
	}
}

// NeedsRenewal 判断会话是否需要开新会话（ctx >= 85%）
func (st *SessionTracker) NeedsRenewal(sessionKey string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if info, ok := st.sessions[sessionKey]; ok {
		return info.ContextUsage >= 85.0
	}
	return false
}

// SetSummary 保存会话摘要
func (st *SessionTracker) SetSummary(sessionKey, summary string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if info, ok := st.sessions[sessionKey]; ok {
		info.Summary = summary
	}
}

// GetSummary 获取会话摘要
func (st *SessionTracker) GetSummary(sessionKey string) string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if info, ok := st.sessions[sessionKey]; ok {
		return info.Summary
	}
	return ""
}

// RenewSession 重置会话的上下文使用率（开新会话后调用）
func (st *SessionTracker) RenewSession(sessionKey string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if info, ok := st.sessions[sessionKey]; ok {
		info.ContextUsage = 0
		info.Renewed = true
	}
}

// ShouldCompress 判断该会话是否需要压缩历史
// 只有上下文使用率超过 70% 才压缩
func (st *SessionTracker) ShouldCompress(sessionKey string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if info, ok := st.sessions[sessionKey]; ok {
		return info.ContextUsage > 70.0
	}
	return false // 新会话不压缩
}

// GetContextUsage 获取会话的上下文使用率
func (st *SessionTracker) GetContextUsage(sessionKey string) float64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if info, ok := st.sessions[sessionKey]; ok {
		return info.ContextUsage
	}
	return 0
}

// SPLICE_2

// extractSessionKey 从请求中提取会话标识
func extractSessionKey(r *http.Request, body map[string]interface{}, messages []interface{}) string {
	apiKey := r.Header.Get("X-Api-Key")
	if apiKey == "" {
		apiKey = r.Header.Get("Authorization")
	}

	// 1. claude-cli: metadata.user_id 里有 session UUID
	if metadata, ok := body["metadata"].(map[string]interface{}); ok {
		if userID, ok := metadata["user_id"].(string); ok && userID != "" {
			// 格式: user_xxx_account__session_uuid
			if idx := strings.Index(userID, "_session_"); idx >= 0 {
				return "cli:" + userID[idx+9:] // session UUID
			}
			return "cli:" + hashStr(userID)
		}
	}

	// 2. openclaw: messages 里有 sender_id（遍历所有 messages，不只是最后一条）
	if len(messages) > 0 {
		// 从后往前找，优先最近的
		for i := len(messages) - 1; i >= 0; i-- {
			if senderID := extractSenderID(messages[i]); senderID != "" {
				return "oc:" + senderID
			}
		}
	}

	// 3. 通用: API Key + system prompt hash + 首条 user message hash（区分不同对话）
	systemHash := ""
	if sys, ok := body["system"]; ok {
		sysBytes, _ := json.Marshal(sys)
		systemHash = hashStr(string(sysBytes))[:12]
	}
	firstMsgHash := ""
	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok {
			if m["role"] == "user" {
				switch c := m["content"].(type) {
				case string:
					if c != "" {
						firstMsgHash = hashStr(c)[:12]
					}
				case []interface{}:
					for _, block := range c {
						if b, ok := block.(map[string]interface{}); ok {
							if t, ok := b["text"].(string); ok && t != "" {
								firstMsgHash = hashStr(t)[:12]
								break
							}
						}
					}
				}
				break
			}
		}
	}
	if apiKey != "" && apiKey != "any-value" {
		return "key:" + hashStr(apiKey)[:12] + ":" + systemHash + ":" + firstMsgHash
	}

	// 4. 兜底: IP + UA + system hash + 首条 user message hash
	return "ip:" + hashStr(r.RemoteAddr+r.UserAgent())[:12] + ":" + systemHash + ":" + firstMsgHash
}

// SPLICE_3

// extractSenderID 从 openclaw 的 message content 里提取 sender_id
func extractSenderID(msg interface{}) string {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return ""
	}
	content := m["content"]
	// content 可能是 string 或 []interface{}
	var texts []string
	switch c := content.(type) {
	case string:
		texts = append(texts, c)
	case []interface{}:
		for _, block := range c {
			if b, ok := block.(map[string]interface{}); ok {
				if t, ok := b["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	// 用 strings.Index 快速定位 sender_id，避免暴力 JSON 解析
	for _, text := range texts {
		idx := strings.Index(text, `"sender_id"`)
		if idx < 0 {
			continue
		}
		// 向前找到包含 sender_id 的 JSON 对象的起始 {
		start := strings.LastIndex(text[:idx], "{")
		if start < 0 {
			continue
		}
		// 从 start 开始找匹配的 }
		depth := 0
		for j := start; j < len(text); j++ {
			if text[j] == '{' {
				depth++
			} else if text[j] == '}' {
				depth--
				if depth == 0 {
					var obj map[string]interface{}
					if json.Unmarshal([]byte(text[start:j+1]), &obj) == nil {
						if sid, ok := obj["sender_id"].(string); ok && sid != "" {
							return sid
						}
					}
					break
				}
			}
		}
	}
	return ""
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}
