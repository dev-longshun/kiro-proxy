package legacy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// APIKeyInfo represents a managed API key
type APIKeyInfo struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
	Enabled     bool      `json:"enabled"`
	RateLimit   int       `json:"rate_limit"` // requests per minute, 0 = unlimited
	TotalUsage  int64     `json:"total_usage"`
	Description string    `json:"description,omitempty"`
}

// KeyManager manages API keys
type KeyManager struct {
	keys     map[string]*APIKeyInfo // key string -> info
	mu       sync.RWMutex
	filePath string
	db       *Database
}

func NewKeyManager(db *Database) *KeyManager {
	km := &KeyManager{
		keys:     make(map[string]*APIKeyInfo),
		filePath: "api_keys.json",
		db:       db,
	}
	km.loadFromDB()
	return km
}

func (km *KeyManager) loadFromDB() {
	if km.db == nil {
		km.loadFromFile()
		return
	}
	keys, err := km.db.LoadAPIKeys()
	if err != nil {
		log.Printf("[KeyManager] DB load error, falling back to file: %v", err)
		km.loadFromFile()
		return
	}
	if len(keys) > 0 {
		km.keys = keys
		log.Printf("[KeyManager] Loaded %d API keys from database", len(keys))
	} else {
		// Try loading from file for migration
		km.loadFromFile()
	}
}

// GenerateKey creates a new API key in Claude-compatible format:
// sk-ant-api03-<40 chars base64url>
func GenerateKey() string {
	b := make([]byte, 30)
	rand.Read(b)
	encoded := base64.RawURLEncoding.EncodeToString(b)
	return "sk-ant-api03-" + encoded
}

// CreateKey creates a new API key. If customKey is non-empty, use it as-is; otherwise auto-generate.
func (km *KeyManager) CreateKey(name, description string, rateLimit int, customKey string) *APIKeyInfo {
	km.mu.Lock()
	defer km.mu.Unlock()

	key := strings.TrimSpace(customKey)
	if key == "" {
		key = GenerateKey()
	}

	// Check duplicate
	if _, exists := km.keys[key]; exists {
		// Append random suffix to avoid collision
		key = key + "-" + GenerateKey()[14:22]
	}

	info := &APIKeyInfo{
		Key:         key,
		Name:        name,
		CreatedAt:   time.Now(),
		Enabled:     true,
		RateLimit:   rateLimit,
		Description: description,
	}
	km.keys[key] = info
	km.saveToFile()
	km.saveKeyToDB(info)

	log.Printf("[KeyManager] Created new key: %s (%s)", name, MaskKey(key))
	return info
}

// ValidateKey checks if a key is valid and enabled
func (km *KeyManager) ValidateKey(key string) bool {
	km.mu.RLock()
	defer km.mu.RUnlock()

	info, ok := km.keys[key]
	if !ok {
		return false
	}
	return info.Enabled
}

// RecordKeyUsage updates usage stats for a key
func (km *KeyManager) RecordKeyUsage(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if info, ok := km.keys[key]; ok {
		info.LastUsedAt = time.Now()
		info.TotalUsage++
	}
}

// GetKey returns key info
func (km *KeyManager) GetKey(key string) *APIKeyInfo {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if info, ok := km.keys[key]; ok {
		return info
	}
	return nil
}

// GetAllKeys returns all keys
func (km *KeyManager) GetAllKeys() []*APIKeyInfo {
	km.mu.RLock()
	defer km.mu.RUnlock()

	result := make([]*APIKeyInfo, 0, len(km.keys))
	for _, info := range km.keys {
		result = append(result, info)
	}
	return result
}

// DeleteKey removes a key
func (km *KeyManager) DeleteKey(key string) bool {
	km.mu.Lock()
	defer km.mu.Unlock()

	if _, ok := km.keys[key]; ok {
		delete(km.keys, key)
		km.saveToFile()
		km.deleteKeyFromDB(key)
		return true
	}
	return false
}

// ToggleKey enables/disables a key
func (km *KeyManager) ToggleKey(key string) (*APIKeyInfo, bool) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if info, ok := km.keys[key]; ok {
		info.Enabled = !info.Enabled
		km.saveToFile()
		km.saveKeyToDB(info)
		return info, true
	}
	return nil, false
}

// UpdateKey updates key properties
func (km *KeyManager) UpdateKey(key, name, description string, rateLimit int) (*APIKeyInfo, bool) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if info, ok := km.keys[key]; ok {
		if name != "" {
			info.Name = name
		}
		if description != "" {
			info.Description = description
		}
		if rateLimit >= 0 {
			info.RateLimit = rateLimit
		}
		km.saveToFile()
		km.saveKeyToDB(info)
		return info, true
	}
	return nil, false
}

// HasKeys returns true if any keys are configured
func (km *KeyManager) HasKeys() bool {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return len(km.keys) > 0
}

// saveToFile persists keys to disk
func (km *KeyManager) saveToFile() {
	// Prefer DB
	if km.db != nil {
		return // DB saves happen inline
	}
	data, err := json.MarshalIndent(km.keys, "", "  ")
	if err != nil {
		log.Printf("[KeyManager] Failed to marshal keys: %v", err)
		return
	}
	if err := os.WriteFile(km.filePath, data, 0644); err != nil {
		log.Printf("[KeyManager] Failed to save keys: %v", err)
	}
}

func (km *KeyManager) saveKeyToDB(info *APIKeyInfo) {
	if km.db != nil {
		km.db.SaveAPIKey(info)
	}
}

func (km *KeyManager) deleteKeyFromDB(key string) {
	if km.db != nil {
		km.db.DeleteAPIKey(key)
	}
}

// loadFromFile loads keys from disk
func (km *KeyManager) loadFromFile() {
	data, err := os.ReadFile(km.filePath)
	if err != nil {
		return
	}
	var keys map[string]*APIKeyInfo
	if err := json.Unmarshal(data, &keys); err != nil {
		log.Printf("[KeyManager] Failed to parse keys file: %v", err)
		return
	}
	km.keys = keys
	log.Printf("[KeyManager] Loaded %d API keys from file", len(keys))
}

// MaskKey masks a key for display: sk-ant-api03-xxxx...xxxx
func MaskKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	if len(key) <= 16 {
		return key[:4] + "..." + key[len(key)-4:]
	}
	// Show prefix + first 8 after prefix + last 4
	prefix := ""
	if len(key) > 20 {
		if idx := strings.LastIndex(key[:20], "-"); idx > 0 {
			prefix = key[:idx+1]
		}
	}
	if prefix == "" {
		prefix = key[:6]
	}
	end := len(prefix) + 4
	if end > len(key) {
		end = len(key)
	}
	return fmt.Sprintf("%s%s...%s", prefix, key[len(prefix):end], key[len(key)-4:])
}
