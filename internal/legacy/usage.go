package legacy

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// UsageRecord represents a single API request usage record
type UsageRecord struct {
	Timestamp    time.Time `json:"timestamp"`
	APIKey       string    `json:"api_key"`
	Model        string    `json:"model"`
	Protocol     string    `json:"protocol"` // openai, anthropic, gemini
	AccountEmail string    `json:"account_email"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	CreditsUsed  float64   `json:"credits_used"`
	Success      bool      `json:"success"`
	DurationMs   int64     `json:"duration_ms"`
}

// UsageSummary provides aggregated usage stats
type UsageSummary struct {
	TotalRequests int64   `json:"total_requests"`
	TotalInput    int64   `json:"total_input_tokens"`
	TotalOutput   int64   `json:"total_output_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	SuccessCount  int64   `json:"success_count"`
	ErrorCount    int64   `json:"error_count"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// KeyUsage tracks usage per API key
type KeyUsage struct {
	APIKey   string        `json:"api_key"`
	KeyName  string        `json:"key_name"`
	Summary  *UsageSummary `json:"summary"`
	LastUsed time.Time     `json:"last_used"`
}

// UsageTracker tracks API usage — writes to memory buffer + DB, reads from DB.
type UsageTracker struct {
	records    []UsageRecord
	pending    []UsageRecord // 待批量写入 DB 的记录
	mu         sync.RWMutex
	maxRecords int
	filePath   string
	db         *Database
}

func NewUsageTracker(db *Database) *UsageTracker {
	ut := &UsageTracker{
		records:    make([]UsageRecord, 0, 1000),
		maxRecords: 10000,
		filePath:   "usage_data.json",
		db:         db,
	}
	return ut
}

// RecordUsage records a new usage entry (memory + batched DB write)
func (ut *UsageTracker) RecordUsage(record UsageRecord) {
	ut.mu.Lock()
	ut.records = append(ut.records, record)
	if len(ut.records) > ut.maxRecords {
		ut.records = ut.records[len(ut.records)-ut.maxRecords:]
	}
	// 攒到 pending 队列，定期批量写入
	ut.pending = append(ut.pending, record)
	shouldFlush := len(ut.pending) >= 20 // 攒够 20 条或定时器触发
	ut.mu.Unlock()

	if shouldFlush {
		go ut.flushPending()
	}
}

// flushPending 批量写入 pending 的 usage 记录
func (ut *UsageTracker) flushPending() {
	ut.mu.Lock()
	if len(ut.pending) == 0 {
		ut.mu.Unlock()
		return
	}
	batch := make([]UsageRecord, len(ut.pending))
	copy(batch, ut.pending)
	ut.pending = ut.pending[:0]
	ut.mu.Unlock()

	if ut.db == nil {
		return
	}

	ut.db.BatchInsertUsage(batch)
}

// GetTotalSummary returns overall usage summary from DB.
func (ut *UsageTracker) GetTotalSummary() *UsageSummary {
	if ut.db != nil {
		if s, err := ut.db.QueryTotalSummary(); err == nil {
			return s
		}
	}
	// Fallback to memory
	return ut.getTotalSummaryFromMemory()
}

// GetSummaryByAccount returns usage summary grouped by account email from DB.
func (ut *UsageTracker) GetSummaryByAccount() map[string]*UsageSummary {
	if ut.db != nil {
		if s, err := ut.db.QuerySummaryByAccount(); err == nil {
			return s
		}
	}
	return ut.getSummaryByAccountFromMemory()
}

// GetSummaryByModel returns usage summary grouped by model from DB.
func (ut *UsageTracker) GetSummaryByModel() map[string]*UsageSummary {
	if ut.db != nil {
		if s, err := ut.db.QuerySummaryByModel(); err == nil {
			return s
		}
	}
	return ut.getSummaryByModelFromMemory()
}

// GetSummaryByKey returns usage summary grouped by API key from DB.
func (ut *UsageTracker) GetSummaryByKey() map[string]*UsageSummary {
	if ut.db != nil {
		if s, err := ut.db.QuerySummaryByKey(); err == nil {
			return s
		}
	}
	return ut.getSummaryByKeyFromMemory()
}

// GetRecentRecords returns the most recent N records from DB.
func (ut *UsageTracker) GetRecentRecords(n int) []UsageRecord {
	if ut.db != nil {
		records, err := ut.db.LoadUsageRecords(n)
		if err == nil && len(records) > 0 {
			result := make([]UsageRecord, len(records))
			for i, r := range records {
				result[i] = *r
			}
			return result
		}
	}
	// Fallback to memory
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	if n > len(ut.records) {
		n = len(ut.records)
	}
	result := make([]UsageRecord, n)
	copy(result, ut.records[len(ut.records)-n:])
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// GetRecords returns all records from memory buffer.
func (ut *UsageTracker) GetRecords() []UsageRecord {
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	return ut.records
}

// ── Memory fallback methods ──

func (ut *UsageTracker) getTotalSummaryFromMemory() *UsageSummary {
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	s := &UsageSummary{}
	for _, r := range ut.records {
		s.TotalRequests++
		s.TotalInput += int64(r.InputTokens)
		s.TotalOutput += int64(r.OutputTokens)
		s.TotalTokens += int64(r.TotalTokens)
		if r.Success {
			s.SuccessCount++
		} else {
			s.ErrorCount++
		}
		s.AvgDurationMs += float64(r.DurationMs)
	}
	if s.TotalRequests > 0 {
		s.AvgDurationMs /= float64(s.TotalRequests)
	}
	return s
}

func (ut *UsageTracker) getSummaryByAccountFromMemory() map[string]*UsageSummary {
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	return aggregateBy(ut.records, func(r UsageRecord) string {
		if r.AccountEmail == "" {
			return "_unknown_"
		}
		return r.AccountEmail
	})
}

func (ut *UsageTracker) getSummaryByModelFromMemory() map[string]*UsageSummary {
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	return aggregateBy(ut.records, func(r UsageRecord) string {
		if r.Model == "" {
			return "unknown"
		}
		return r.Model
	})
}

func (ut *UsageTracker) getSummaryByKeyFromMemory() map[string]*UsageSummary {
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	return aggregateBy(ut.records, func(r UsageRecord) string {
		if r.APIKey == "" {
			return "_no_key_"
		}
		return r.APIKey
	})
}

func aggregateBy(records []UsageRecord, keyFn func(UsageRecord) string) map[string]*UsageSummary {
	summaries := make(map[string]*UsageSummary)
	for _, r := range records {
		key := keyFn(r)
		s, ok := summaries[key]
		if !ok {
			s = &UsageSummary{}
			summaries[key] = s
		}
		s.TotalRequests++
		s.TotalInput += int64(r.InputTokens)
		s.TotalOutput += int64(r.OutputTokens)
		s.TotalTokens += int64(r.TotalTokens)
		if r.Success {
			s.SuccessCount++
		} else {
			s.ErrorCount++
		}
		s.AvgDurationMs += float64(r.DurationMs)
	}
	for _, s := range summaries {
		if s.TotalRequests > 0 {
			s.AvgDurationMs /= float64(s.TotalRequests)
		}
	}
	return summaries
}

// EstimateTokens estimates input/output tokens from request/response
func EstimateTokens(inputData interface{}, outputContent string) (int, int) {
	inputBytes, _ := json.Marshal(inputData)
	inputTokens := len(inputBytes) / 4 // ~4 chars per token
	outputTokens := len(outputContent) / 4
	return inputTokens, outputTokens
}

// SaveToFile persists usage data to disk
func (ut *UsageTracker) SaveToFile() {
	ut.mu.RLock()
	defer ut.mu.RUnlock()
	data, err := json.Marshal(ut.records)
	if err != nil {
		log.Printf("[Usage] Failed to marshal usage data: %v", err)
		return
	}
	if err := os.WriteFile(ut.filePath, data, 0644); err != nil {
		log.Printf("[Usage] Failed to save usage data: %v", err)
	}
}

// StartPeriodicSave starts a goroutine that saves usage data periodically
func (ut *UsageTracker) StartPeriodicSave() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Usage] panic in periodic save: %v", r)
			}
		}()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if ut.db == nil {
				ut.SaveToFile()
			}
		}
	}()
	// 定期 flush pending usage 到 DB（每 10 秒）
	if ut.db != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Usage] panic in flush pending: %v", r)
				}
			}()
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				ut.flushPending()
			}
		}()
	}
}
