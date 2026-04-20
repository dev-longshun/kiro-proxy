package proxy

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// MetricsTracker 轻量级实时指标追踪器
type MetricsTracker struct {
	// 当前窗口的原子计数器（每秒重置）
	currentRequests     int64
	currentInputTokens  int64
	currentOutputTokens int64

	// 最近 60 秒的快照
	snapshots   [60]MetricsSnapshot
	snapshotIdx int
	snapshotMu  sync.RWMutex

	// 总计（不重置）
	totalRequests     int64
	totalInputTokens  int64
	totalOutputTokens int64
	startTime         time.Time

	stopCh chan struct{}
}

// MetricsSnapshot 每秒快照
type MetricsSnapshot struct {
	Timestamp    int64 `json:"ts"`
	Requests     int64 `json:"req"`
	InputTokens  int64 `json:"in"`
	OutputTokens int64 `json:"out"`
}

func NewMetricsTracker() *MetricsTracker {
	m := &MetricsTracker{
		startTime: time.Now(),
		stopCh:    make(chan struct{}),
	}
	go m.tickLoop()
	return m
}

// Stop 停止 metrics 采集 goroutine
func (m *MetricsTracker) Stop() {
	close(m.stopCh)
}

// RecordRequest 记录一次请求完成（在请求结束时调用）
func (m *MetricsTracker) RecordRequest(inputTokens, outputTokens int) {
	atomic.AddInt64(&m.currentRequests, 1)
	atomic.AddInt64(&m.currentInputTokens, int64(inputTokens))
	atomic.AddInt64(&m.currentOutputTokens, int64(outputTokens))
	atomic.AddInt64(&m.totalRequests, 1)
	atomic.AddInt64(&m.totalInputTokens, int64(inputTokens))
	atomic.AddInt64(&m.totalOutputTokens, int64(outputTokens))
}

// tickLoop 每秒快照一次，重置当前窗口计数器
func (m *MetricsTracker) tickLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Metrics] panic in tickLoop: %v", r)
		}
	}()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			snap := MetricsSnapshot{
				Timestamp:    time.Now().Unix(),
				Requests:     atomic.SwapInt64(&m.currentRequests, 0),
				InputTokens:  atomic.SwapInt64(&m.currentInputTokens, 0),
				OutputTokens: atomic.SwapInt64(&m.currentOutputTokens, 0),
			}
			m.snapshotMu.Lock()
			m.snapshots[m.snapshotIdx] = snap
			m.snapshotIdx = (m.snapshotIdx + 1) % 60
			m.snapshotMu.Unlock()
		}
	}
}

// GetMetrics 返回实时指标数据
func (m *MetricsTracker) GetMetrics(activeRequests int64, queuedRequests int64) map[string]interface{} {
	m.snapshotMu.RLock()
	defer m.snapshotMu.RUnlock()

	// 收集最近 60 秒快照（按时间顺序）
	history := make([]MetricsSnapshot, 0, 60)
	for i := 0; i < 60; i++ {
		idx := (m.snapshotIdx + i) % 60
		s := m.snapshots[idx]
		if s.Timestamp > 0 {
			history = append(history, s)
		}
	}

	// 计算最近 1 秒的 RPS
	var rps, inPerSec, outPerSec int64
	if len(history) > 0 {
		last := history[len(history)-1]
		rps = last.Requests
		inPerSec = last.InputTokens
		outPerSec = last.OutputTokens
	}

	// 计算最近 60 秒的 RPM
	var rpm, inPerMin, outPerMin int64
	for _, s := range history {
		rpm += s.Requests
		inPerMin += s.InputTokens
		outPerMin += s.OutputTokens
	}

	return map[string]interface{}{
		"rps":             rps,
		"rpm":             rpm,
		"input_tokens_s":  inPerSec,
		"output_tokens_s": outPerSec,
		"input_tokens_m":  inPerMin,
		"output_tokens_m": outPerMin,
		"active":          activeRequests,
		"queued":          queuedRequests,
		"total_requests":  atomic.LoadInt64(&m.totalRequests),
		"total_input":     atomic.LoadInt64(&m.totalInputTokens),
		"total_output":    atomic.LoadInt64(&m.totalOutputTokens),
		"uptime_seconds":  int64(time.Since(m.startTime).Seconds()),
		"history":         history,
	}
}
