package proxy

import (
	"database/sql"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
)

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	v, _ := strconv.Atoi(s)
	return v
}

// LoadAccountsFromDB loads accounts from the legacy database
func LoadAccountsFromDB(d *legacy.Database) ([]*Account, error) {
	rows, err := d.QueryAccounts()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []*Account
	for rows.Next() {
		var (
			id, email, nickname, idp, status string
			enabled, maxConc                 int
			proxyID, machineID               string
			supportedModelsStr               string
			credsJSON, limitsJSON            string
			creditsUsedS, lastCreditsS       sql.NullString
			ctxPctS                          sql.NullString
			reqCountS, errCountS             sql.NullString
			consErrsS, lastErrCodeS          sql.NullString
			lastErrMsg, suspAt, suspReason   string
			lastUsed                         string
			totalSuccess, total429, totalErr int64
		)
		err := rows.Scan(&id, &email, &nickname, &idp, &status, &enabled,
			&maxConc, &proxyID, &machineID, &supportedModelsStr, &credsJSON, &limitsJSON,
			&creditsUsedS, &lastCreditsS, &ctxPctS,
			&reqCountS, &errCountS, &consErrsS,
			&lastErrCodeS, &lastErrMsg, &suspAt, &suspReason,
			&lastUsed,
			&totalSuccess, &total429, &totalErr)
		if err != nil {
			log.Printf("[DB] Error scanning account: %v", err)
			continue
		}

		// 安全解析数值字段（容忍空字符串和非数值数据）
		creditsUsed := parseFloat(creditsUsedS.String)
		lastCredits := parseFloat(lastCreditsS.String)
		ctxPct := parseFloat(ctxPctS.String)
		reqCount := parseInt64(reqCountS.String)
		errCount := parseInt64(errCountS.String)
		consErrs := parseInt(consErrsS.String)
		lastErrCode := parseInt(lastErrCodeS.String)

		// 解析支持的模型列表
		var supportedModels []string
		if supportedModelsStr != "" {
			for _, m := range strings.Split(supportedModelsStr, ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					supportedModels = append(supportedModels, m)
				}
			}
		}

		acc := &Account{
			ID:                  id,
			Email:               email,
			Nickname:            nickname,
			IDP:                 idp,
			MachineID:           machineID,
			SupportedModels:     supportedModels,
			Status:              kiro.CredentialStatus(status),
			Enabled:             enabled == 1,
			MaxConcurrent:       maxConc,
			ProxyID:             proxyID,
			CreditsUsed:         creditsUsed,
			LastCreditsUsed:     lastCredits,
			ContextUsagePercent: ctxPct,
			RequestCount:        reqCount,
			ErrorCount:          errCount,
			ConsecutiveErrs:     consErrs,
			LastErrorCode:       lastErrCode,
			LastErrorMessage:    lastErrMsg,
			SuspendedReason:     suspReason,
			TotalRequestsOK:    totalSuccess,
			TotalRequests429:    total429,
			TotalRequestsErr:    totalErr,
		}

		if credsJSON != "" && credsJSON != "{}" {
			var creds kiro.KiroCredentials
			if json.Unmarshal([]byte(credsJSON), &creds) == nil {
				acc.Credentials = &creds
			}
		}
		if limitsJSON != "" {
			var limits kiro.KiroUsageLimits
			if json.Unmarshal([]byte(limitsJSON), &limits) == nil {
				acc.UsageLimits = &limits
			}
		}
		if suspAt != "" {
			acc.SuspendedAt, _ = time.Parse(time.RFC3339, suspAt)
		}
		if lastUsed != "" {
			acc.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed)
		}

		// 确保每个账号有独立的 machineId
		if acc.MachineID == "" {
			acc.MachineID = kiro.GenerateRandomMachineID()
		}

		accounts = append(accounts, acc)
	}
	return accounts, nil
}

// accountDBRow holds the serialized fields for DB persistence.
type accountDBRow struct {
	credsJSON  string
	limitsJSON string
	suspAt     string
	lastUsed   string
}

// serializeAccount extracts DB-ready fields from an Account (caller must hold acc.Mu).
func serializeAccount(acc *Account) accountDBRow {
	row := accountDBRow{}
	if b, err := json.Marshal(acc.Credentials); err == nil {
		row.credsJSON = string(b)
	}
	if acc.UsageLimits != nil {
		if b, err := json.Marshal(acc.UsageLimits); err == nil {
			row.limitsJSON = string(b)
		}
	}
	if !acc.SuspendedAt.IsZero() {
		row.suspAt = acc.SuspendedAt.Format(time.RFC3339)
	}
	if !acc.LastUsedAt.IsZero() {
		row.lastUsed = acc.LastUsedAt.Format(time.RFC3339)
	}
	return row
}

// accountDBArgs returns the ordered parameter list for the INSERT statement.
func accountDBArgs(acc *Account, row accountDBRow) []interface{} {
	modelsStr := strings.Join(acc.SupportedModels, ",")
	return []interface{}{
		acc.ID, acc.Email, acc.Nickname, acc.IDP,
		string(acc.Status), legacy.BoolToInt(acc.Enabled), acc.MaxConcurrent, acc.ProxyID,
		acc.MachineID, modelsStr, row.credsJSON, row.limitsJSON, acc.CreditsUsed, acc.LastCreditsUsed,
		acc.ContextUsagePercent, acc.RequestCount, acc.ErrorCount, acc.ConsecutiveErrs,
		acc.LastErrorCode, acc.LastErrorMessage, row.suspAt, acc.SuspendedReason,
		acc.ID, row.lastUsed,
		atomic.LoadInt64(&acc.TotalRequestsOK), atomic.LoadInt64(&acc.TotalRequests429), atomic.LoadInt64(&acc.TotalRequestsErr),
	}
}

const accountInsertSQL = `REPLACE INTO accounts
	(id, email, nickname, idp, status, enabled, max_concurrent, proxy_id,
	 machine_id, supported_models, credentials_json, usage_limits_json, credits_used, last_credits_used,
	 context_usage_percent, request_count, error_count, consecutive_errs,
	 last_error_code, last_error_message, suspended_at, suspended_reason,
	 created_at, last_used_at, total_success, total_429, total_errors)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// SaveAccountToDB saves a single account to the legacy database
func SaveAccountToDB(d *legacy.Database, acc *Account) error {
	acc.Mu.Lock()
	row := serializeAccount(acc)
	args := accountDBArgs(acc, row)
	acc.Mu.Unlock()

	return d.ExecAccountInsert(accountInsertSQL, args...)
}

// SaveAllAccountsToDB saves all accounts to DB (batch)
func SaveAllAccountsToDB(d *legacy.Database, accounts []*Account) error {
	batch, err := d.BeginAccountBatch()
	if err != nil {
		return err
	}

	for _, acc := range accounts {
		acc.Mu.Lock()
		row := serializeAccount(acc)
		args := accountDBArgs(acc, row)
		acc.Mu.Unlock()

		if err := batch.Exec(args...); err != nil {
			log.Printf("[DB] Error saving account %s: %v", acc.Email, err)
		}
	}

	return batch.Commit()
}

// MigrateAccountsFromFile imports accounts from the old JSON accounts file
func MigrateAccountsFromFile(d *legacy.Database, accounts []*Account) int {
	if d.QueryAccountCount() > 0 {
		return 0 // already have accounts in DB
	}

	imported := 0
	for _, acc := range accounts {
		if SaveAccountToDB(d, acc) == nil {
			imported++
		}
	}
	if imported > 0 {
		log.Printf("[Migration] Imported %d accounts into database", imported)
	}
	return imported
}
