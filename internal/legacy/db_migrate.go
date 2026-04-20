package legacy

import (
	"encoding/json"
	"log"
	"os"
	"time"
)

// MigrateFromJSON imports data from old JSON files into the database
func (d *Database) MigrateFromJSON() {
	d.migrateAPIKeys()
	d.migrateUsageData()
	d.migrateProxies()
}

func (d *Database) migrateAPIKeys() {
	path := "api_keys.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return // no file
	}
	var keys map[string]*APIKeyInfo
	if json.Unmarshal(data, &keys) != nil {
		return
	}
	count := 0
	for _, info := range keys {
		if d.SaveAPIKey(info) == nil {
			count++
		}
	}
	if count > 0 {
		log.Printf("[Migration] Imported %d API keys from %s", count, path)
		os.Rename(path, path+".bak")
	}
}

func (d *Database) migrateUsageData() {
	path := "usage_data.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var records []*UsageRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	if len(records) == 0 {
		return
	}

	// 使用事务批量插入，避免逐条 fsync 导致启动缓慢
	d.mu.Lock()
	tx, err := d.db.Begin()
	if err != nil {
		d.mu.Unlock()
		log.Printf("[Migration] 开启事务失败: %v", err)
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_records
		(timestamp, api_key, model, protocol, account_email,
		 input_tokens, output_tokens, total_tokens, success, duration_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		d.mu.Unlock()
		log.Printf("[Migration] 准备语句失败: %v", err)
		return
	}
	defer stmt.Close()

	count := 0
	for _, r := range records {
		_, err := stmt.Exec(
			r.Timestamp.Format(time.RFC3339), r.APIKey, r.Model, r.Protocol, r.AccountEmail,
			r.InputTokens, r.OutputTokens, r.TotalTokens,
			BoolToInt(r.Success), r.DurationMs,
		)
		if err == nil {
			count++
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		d.mu.Unlock()
		log.Printf("[Migration] 提交事务失败: %v", err)
		return
	}
	d.mu.Unlock()

	if count > 0 {
		log.Printf("[Migration] Imported %d usage records from %s", count, path)
		os.Rename(path, path+".bak")
	}
}

func (d *Database) migrateProxies() {
	path := "proxies.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var proxies []*ProxyInfo
	if json.Unmarshal(data, &proxies) != nil {
		return
	}
	count := 0
	for _, p := range proxies {
		if d.SaveProxy(p) == nil {
			count++
		}
	}
	if count > 0 {
		log.Printf("[Migration] Imported %d proxies from %s", count, path)
		os.Rename(path, path+".bak")
	}
}

// StartPeriodicSync starts a goroutine that periodically calls the sync function
func (d *Database) StartPeriodicSync(syncFunc func() error, interval time.Duration) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[DB] panic in periodic sync: %v", r)
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if err := syncFunc(); err != nil {
				log.Printf("[DB] Periodic sync error: %v", err)
			}
		}
	}()
}

// Shutdown saves final state and closes the database
func (d *Database) Shutdown(finalSave func() error) {
	log.Println("[DB] Saving final state...")
	if err := finalSave(); err != nil {
		log.Printf("[DB] Final save error: %v", err)
	}
	d.Close()
	log.Println("[DB] Database closed")
}
