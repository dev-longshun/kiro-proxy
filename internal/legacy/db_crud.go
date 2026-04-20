package legacy

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// ==================== API Keys ====================

func (d *Database) SaveAPIKey(info *APIKeyInfo) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("REPLACE INTO api_keys (`key`, name, description, enabled, rate_limit, total_usage, created_at, last_used_at) VALUES (?,?,?,?,?,?,?,?)",
		info.Key, info.Name, info.Description, BoolToInt(info.Enabled),
		info.RateLimit, info.TotalUsage,
		info.CreatedAt.Format(time.RFC3339), info.LastUsedAt.Format(time.RFC3339),
	)
	return err
}

func (d *Database) LoadAPIKeys() (map[string]*APIKeyInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query("SELECT `key`, name, description, enabled, rate_limit, total_usage, created_at, last_used_at FROM api_keys")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make(map[string]*APIKeyInfo)
	for rows.Next() {
		var key, name, desc, createdAt, lastUsed string
		var enabled, rateLimit int
		var totalUsage int64
		if err := rows.Scan(&key, &name, &desc, &enabled, &rateLimit, &totalUsage, &createdAt, &lastUsed); err != nil {
			continue
		}
		info := &APIKeyInfo{
			Key:         key,
			Name:        name,
			Description: desc,
			Enabled:     enabled == 1,
			RateLimit:   rateLimit,
			TotalUsage:  totalUsage,
		}
		info.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		info.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed)
		keys[key] = info
	}
	return keys, nil
}

func (d *Database) DeleteAPIKey(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM api_keys WHERE `key` = ?", key)
	return err
}

// ==================== Usage Records ====================

func (d *Database) InsertUsageRecord(r *UsageRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`INSERT INTO usage_records
		(timestamp, api_key, model, protocol, account_email,
		 input_tokens, output_tokens, total_tokens, success, duration_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.Timestamp.Format(time.RFC3339), r.APIKey, r.Model, r.Protocol, r.AccountEmail,
		r.InputTokens, r.OutputTokens, r.TotalTokens,
		BoolToInt(r.Success), r.DurationMs,
	)
	return err
}

func (d *Database) LoadUsageRecords(limit int) ([]*UsageRecord, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	query := "SELECT timestamp, api_key, model, protocol, account_email, input_tokens, output_tokens, total_tokens, success, duration_ms FROM usage_records ORDER BY id DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []*UsageRecord
	for rows.Next() {
		var ts, apiKey, model, protocol, email string
		var inTok, outTok, totalTok int
		var success int
		var durationMs int64
		if err := rows.Scan(&ts, &apiKey, &model, &protocol, &email, &inTok, &outTok, &totalTok, &success, &durationMs); err != nil {
			continue
		}
		r := &UsageRecord{
			APIKey:       apiKey,
			Model:        model,
			Protocol:     protocol,
			AccountEmail: email,
			InputTokens:  inTok,
			OutputTokens: outTok,
			TotalTokens:  totalTok,
			Success:      success == 1,
			DurationMs:   durationMs,
		}
		r.Timestamp, _ = time.Parse(time.RFC3339, ts)
		records = append(records, r)
	}
	return records, nil
}

func (d *Database) GetUsageSummary() (map[string]map[string]interface{}, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT account_email,
		COUNT(*) as total, SUM(CASE WHEN success=1 THEN 1 ELSE 0 END) as ok,
		SUM(input_tokens) as in_tok, SUM(output_tokens) as out_tok,
		SUM(credits_used) as credits
		FROM usage_records GROUP BY account_email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]map[string]interface{})
	for rows.Next() {
		var email string
		var total, ok, inTok, outTok int
		var credits float64
		if err := rows.Scan(&email, &total, &ok, &inTok, &outTok, &credits); err != nil {
			continue
		}
		result[email] = map[string]interface{}{
			"total_requests": total,
			"success":        ok,
			"errors":         total - ok,
			"input_tokens":   inTok,
			"output_tokens":  outTok,
			"credits":        credits,
		}
	}
	return result, nil
}

func (d *Database) GetUsageCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var count int
	d.db.QueryRow("SELECT COUNT(*) FROM usage_records").Scan(&count)
	return count
}

// ==================== Proxies ====================

func (d *Database) SaveProxy(p *ProxyInfo) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	boundJSON, _ := json.Marshal(p.BoundAccounts)
	lastUsed := ""
	if !p.LastUsedAt.IsZero() {
		lastUsed = p.LastUsedAt.Format(time.RFC3339)
	}
	_, err := d.db.Exec(`REPLACE INTO proxies
		(id, name, url, type, enabled, max_accounts, expires_at,
		 bound_accounts_json, success_count, error_count, last_used_at,
		 last_error, last_latency_ms, last_test_ip, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.URL, p.Type, BoolToInt(p.Enabled), p.MaxAccounts, p.ExpiresAt,
		string(boundJSON), p.SuccessCount, p.ErrorCount, lastUsed,
		p.LastError, p.LastLatency, p.LastTestIP, p.CreatedAt.Format(time.RFC3339),
	)
	return err
}

func (d *Database) LoadProxies() ([]*ProxyInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT id, name, url, type, enabled, max_accounts, expires_at,
		bound_accounts_json, success_count, error_count, last_used_at,
		last_error, last_latency_ms, last_test_ip, created_at FROM proxies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var proxies []*ProxyInfo
	for rows.Next() {
		var id, name, url, ptype, expiresAt, boundJSON string
		var enabled, maxAcc int
		var successCnt, errorCnt, latency int64
		var lastUsed, lastErr, testIP, createdAt string
		if err := rows.Scan(&id, &name, &url, &ptype, &enabled, &maxAcc, &expiresAt,
			&boundJSON, &successCnt, &errorCnt, &lastUsed,
			&lastErr, &latency, &testIP, &createdAt); err != nil {
			continue
		}
		p := &ProxyInfo{
			ID: id, Name: name, URL: url, Type: ptype,
			Enabled: enabled == 1, MaxAccounts: maxAcc, ExpiresAt: expiresAt,
			SuccessCount: successCnt, ErrorCount: errorCnt,
			LastError: lastErr, LastLatency: latency, LastTestIP: testIP,
		}
		json.Unmarshal([]byte(boundJSON), &p.BoundAccounts)
		if p.BoundAccounts == nil {
			p.BoundAccounts = []string{}
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if lastUsed != "" {
			p.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed)
		}
		proxies = append(proxies, p)
	}
	return proxies, nil
}

func (d *Database) DeleteProxy(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM proxies WHERE id = ?", id)
	return err
}

// ==================== Settings ====================

func (d *Database) GetSetting(key string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var val string
	d.db.QueryRow("SELECT value FROM settings WHERE `key` = ?", key).Scan(&val)
	return val
}

func (d *Database) SetSetting(key, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("REPLACE INTO settings (`key`, value) VALUES (?, ?)", key, value)
	return err
}

// ==================== Helpers ====================

// BoolToInt converts a bool to int (1 or 0)
func BoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpdateAccountField updates a single field on an account row
func (d *Database) UpdateAccountField(id, field string, value interface{}) error {
	if !allowedAccountFields[field] {
		return fmt.Errorf("invalid field name: %s", field)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	query := fmt.Sprintf("UPDATE accounts SET %s = ? WHERE id = ?", field)
	_, err := d.db.Exec(query, value, id)
	return err
}

// allowedAccountFields is a whitelist of columns that can be updated via UpdateAccountField
var allowedAccountFields = map[string]bool{
	"email": true, "nickname": true, "idp": true, "status": true,
	"enabled": true, "max_concurrent": true, "proxy_id": true,
	"credentials_json": true, "usage_limits_json": true,
	"credits_used": true, "last_credits_used": true, "context_usage_percent": true,
	"request_count": true, "error_count": true, "consecutive_errs": true,
	"last_error_code": true, "last_error_message": true,
	"suspended_at": true, "suspended_reason": true, "last_used_at": true,
}

// DeleteAccount deletes an account by ID
func (d *Database) DeleteAccount(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM accounts WHERE id = ?", id)
	return err
}

// UpdateAccountCredentials updates credentials JSON for an account
func (d *Database) UpdateAccountCredentials(id string, credsJSON []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE accounts SET credentials_json = ? WHERE id = ?", string(credsJSON), id)
	return err
}

// UpdateAccountStatus updates the status of an account
func (d *Database) UpdateAccountStatus(id string, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE accounts SET status = ? WHERE id = ?", status, id)
	return err
}

// ExecAccountInsert executes a raw account insert/replace statement
func (d *Database) ExecAccountInsert(query string, args ...interface{}) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(query, args...)
	return err
}

// QueryAccounts returns raw rows for account loading
func (d *Database) QueryAccounts() (*RawRows, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT id, email, nickname, idp, status, enabled,
		max_concurrent, proxy_id, machine_id, supported_models, credentials_json, usage_limits_json,
		credits_used, last_credits_used, context_usage_percent,
		request_count, error_count, consecutive_errs,
		last_error_code, last_error_message, suspended_at, suspended_reason,
		last_used_at,
		COALESCE(total_success, 0), COALESCE(total_429, 0), COALESCE(total_errors, 0)
		FROM accounts`)
	if err != nil {
		return nil, err
	}
	return &RawRows{Rows: rows}, nil
}

// RawRows wraps sql.Rows for external scanning
type RawRows struct {
	Rows interface{ Next() bool; Scan(dest ...interface{}) error; Close() error }
}

func (r *RawRows) Next() bool                          { return r.Rows.Next() }
func (r *RawRows) Scan(dest ...interface{}) error       { return r.Rows.Scan(dest...) }
func (r *RawRows) Close() error                         { return r.Rows.Close() }

// QueryAccountCount returns the number of accounts in the database
func (d *Database) QueryAccountCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var count int
	d.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	return count
}

// BeginAccountBatch starts a transaction for batch account operations
func (d *Database) BeginAccountBatch() (*AccountBatch, error) {
	d.mu.Lock()
	tx, err := d.db.Begin()
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	stmt, err := tx.Prepare(`REPLACE INTO accounts
		(id, email, nickname, idp, status, enabled, max_concurrent, proxy_id,
		 machine_id, supported_models, credentials_json, usage_limits_json, credits_used, last_credits_used,
		 context_usage_percent, request_count, error_count, consecutive_errs,
		 last_error_code, last_error_message, suspended_at, suspended_reason,
		 created_at, last_used_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		d.mu.Unlock()
		return nil, err
	}
	return &AccountBatch{tx: tx, stmt: stmt, mu: &d.mu}, nil
}

// AccountBatch wraps a transaction for batch account inserts
type AccountBatch struct {
	tx   *sql.Tx
	stmt *sql.Stmt
	mu   sync.Locker
}

func (b *AccountBatch) Exec(args ...interface{}) error {
	_, err := b.stmt.Exec(args...)
	return err
}

func (b *AccountBatch) Commit() error {
	defer b.mu.Unlock()
	b.stmt.Close()
	return b.tx.Commit()
}

func (b *AccountBatch) Rollback() {
	defer b.mu.Unlock()
	b.stmt.Close()
	b.tx.Rollback()
}

// ==================== 模型-账号映射 ====================

// SaveModelAccounts 保存某个模型的账号列表（先删后插）
func (d *Database) SaveModelAccounts(model string, accountIDs []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	tx.Exec("DELETE FROM model_accounts WHERE model = ?", model)
	for _, id := range accountIDs {
		tx.Exec("REPLACE INTO model_accounts (model, account_id) VALUES (?, ?)", model, id)
	}
	return tx.Commit()
}

// LoadModelAccounts 加载所有模型-账号映射
func (d *Database) LoadModelAccounts() (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query("SELECT model, account_id FROM model_accounts ORDER BY model")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]string)
	for rows.Next() {
		var model, accID string
		if err := rows.Scan(&model, &accID); err != nil {
			continue
		}
		result[model] = append(result[model], accID)
	}
	return result, nil
}

// DeleteModelAccounts 删除某个模型的所有账号映射
func (d *Database) DeleteModelAccounts(model string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM model_accounts WHERE model = ?", model)
	return err
}

// BatchInsertUsage 批量插入 usage 记录
func (d *Database) BatchInsertUsage(records []UsageRecord) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		log.Printf("[Usage] batch insert begin failed: %v", err)
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_records (timestamp, api_key, model, protocol, account_email, input_tokens, output_tokens, total_tokens, credits_used, success, duration_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		log.Printf("[Usage] batch insert prepare failed: %v", err)
		return
	}
	defer stmt.Close()
	for _, r := range records {
		stmt.Exec(r.Timestamp, r.APIKey, r.Model, r.Protocol, r.AccountEmail, r.InputTokens, r.OutputTokens, r.TotalTokens, r.CreditsUsed, r.Success, r.DurationMs)
	}
	tx.Commit()
}

// Unused import guard
var _ = log.Printf
