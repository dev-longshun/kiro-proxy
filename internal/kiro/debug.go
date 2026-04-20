package kiro

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

var bodySeq uint64

// debugSaveEnabled controls whether request/response bodies are saved to disk.
// 0 = disabled (default), 1 = enabled.
var debugSaveEnabled int32

// SetDebugSave enables or disables saving request/response bodies to logs/ directory.
func SetDebugSave(enabled bool) {
	if enabled {
		atomic.StoreInt32(&debugSaveEnabled, 1)
		log.Printf("[Debug] Body save to disk: ENABLED")
	} else {
		atomic.StoreInt32(&debugSaveEnabled, 0)
		log.Printf("[Debug] Body save to disk: DISABLED")
	}
}

// IsDebugSaveEnabled returns whether body saving is currently enabled.
func IsDebugSaveEnabled() bool {
	return atomic.LoadInt32(&debugSaveEnabled) == 1
}

// SaveBodyToFile saves request/response body to logs/ directory for debugging.
// 受 debugSaveEnabled 开关控制（默认关闭）
func SaveBodyToFile(prefix string, data []byte) {
	if !IsDebugSaveEnabled() {
		return
	}

	logsDir := "logs"
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return
	}

	seq := atomic.AddUint64(&bodySeq, 1)
	ts := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("%s_%03d_%s.json", ts, seq, prefix)
	path := filepath.Join(logsDir, filename)

	var prettyData []byte
	var raw interface{}
	if json.Unmarshal(data, &raw) == nil {
		prettyData, _ = json.MarshalIndent(raw, "", "  ")
	} else {
		prettyData = data
	}

	os.WriteFile(path, prettyData, 0644)
}
