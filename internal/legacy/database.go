package legacy

import (
	"fmt"
	"log"
	"strings"
	"sync"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Database wraps sqlx.DB with helper methods for all entities
type Database struct {
	db *sqlx.DB
	mu sync.RWMutex
}

// GetSqlxDB returns the underlying *sqlx.DB for sharing with other layers
func (d *Database) GetSqlxDB() *sqlx.DB {
	return d.db
}

// OpenDatabase connects to MySQL and auto-creates database + tables
func OpenDatabase(dsn string) (*Database, error) {
	if dsn == "" {
		dsn = "root:password@tcp(127.0.0.1:3306)/kiro_proxy?charset=utf8mb4&parseTime=true&loc=Local"
	}

	// 自动建库：先连不带数据库名的地址，CREATE DATABASE，再连完整 DSN
	if err := ensureDatabase(dsn); err != nil {
		log.Printf("[DB] 自动建库失败（可能已存在）: %v", err)
	}

	db, err := sqlx.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	d := &Database{db: db}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	log.Printf("[DB] MySQL 连接成功")
	return d, nil
}

// ensureDatabase 从 DSN 中提取数据库名，先连 MySQL 建库
func ensureDatabase(dsn string) error {
	// DSN 格式: user:pass@tcp(host:port)/dbname?params
	// 提取 dbname
	slashIdx := strings.Index(dsn, "/")
	if slashIdx < 0 {
		return nil
	}
	afterSlash := dsn[slashIdx+1:]
	dbName := afterSlash
	if qIdx := strings.Index(afterSlash, "?"); qIdx >= 0 {
		dbName = afterSlash[:qIdx]
	}
	if dbName == "" {
		return nil
	}

	// 构造不带数据库名的 DSN
	params := ""
	if qIdx := strings.Index(afterSlash, "?"); qIdx >= 0 {
		params = afterSlash[qIdx:]
	}
	rootDSN := dsn[:slashIdx+1] + params

	rootDB, err := sqlx.Open("mysql", rootDSN)
	if err != nil {
		return err
	}
	defer rootDB.Close()

	_, err = rootDB.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName))
	if err != nil {
		return err
	}
	log.Printf("[DB] 数据库 %s 已确认存在", dbName)
	return nil
}

// Close closes the database
func (d *Database) Close() error {
	return d.db.Close()
}

func (d *Database) migrate() error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id VARCHAR(128) PRIMARY KEY,
			email VARCHAR(255) NOT NULL DEFAULT '',
			nickname VARCHAR(255) DEFAULT '',
			idp VARCHAR(64) DEFAULT '',
			status VARCHAR(32) DEFAULT 'active',
			enabled TINYINT DEFAULT 1,
			max_concurrent INT DEFAULT 2,
			proxy_id VARCHAR(128) DEFAULT '',
			machine_id VARCHAR(255) DEFAULT '',
			supported_models TEXT,
			credentials_json LONGTEXT,
			usage_limits_json TEXT,
			credits_used DOUBLE DEFAULT 0,
			last_credits_used DOUBLE DEFAULT 0,
			context_usage_percent DOUBLE DEFAULT 0,
			request_count INT DEFAULT 0,
			error_count INT DEFAULT 0,
			consecutive_errs INT DEFAULT 0,
			last_error_code INT DEFAULT 0,
			last_error_message TEXT,
			suspended_at VARCHAR(64) DEFAULT '',
			suspended_reason TEXT,
			created_at VARCHAR(64) NOT NULL DEFAULT '',
			last_used_at VARCHAR(64) DEFAULT ''
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS api_keys (
			` + "`key`" + ` VARCHAR(255) PRIMARY KEY,
			name VARCHAR(255) DEFAULT '',
			description TEXT,
			enabled TINYINT DEFAULT 1,
			rate_limit INT DEFAULT 0,
			total_usage INT DEFAULT 0,
			created_at VARCHAR(64) NOT NULL DEFAULT '',
			last_used_at VARCHAR(64) DEFAULT ''
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS usage_records (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			timestamp VARCHAR(64) NOT NULL DEFAULT '',
			api_key VARCHAR(255) DEFAULT '',
			model VARCHAR(128) DEFAULT '',
			protocol VARCHAR(32) DEFAULT '',
			account_email VARCHAR(255) DEFAULT '',
			input_tokens INT DEFAULT 0,
			output_tokens INT DEFAULT 0,
			total_tokens INT DEFAULT 0,
			success TINYINT DEFAULT 1,
			duration_ms INT DEFAULT 0,
			credits_used DOUBLE DEFAULT 0,
			INDEX idx_usage_timestamp (timestamp),
			INDEX idx_usage_account (account_email),
			INDEX idx_usage_model (model)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS proxies (
			id VARCHAR(128) PRIMARY KEY,
			name VARCHAR(255) DEFAULT '',
			url TEXT NOT NULL,
			type VARCHAR(32) DEFAULT 'socks5',
			enabled TINYINT DEFAULT 1,
			max_accounts INT DEFAULT 0,
			expires_at VARCHAR(64) DEFAULT '',
			bound_accounts_json TEXT,
			success_count INT DEFAULT 0,
			error_count INT DEFAULT 0,
			last_used_at VARCHAR(64) DEFAULT '',
			last_error TEXT,
			last_latency_ms INT DEFAULT 0,
			last_test_ip VARCHAR(64) DEFAULT '',
			created_at VARCHAR(64) NOT NULL DEFAULT ''
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS settings (
			` + "`key`" + ` VARCHAR(255) PRIMARY KEY,
			value LONGTEXT
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS model_accounts (
			model VARCHAR(128) NOT NULL,
			account_id VARCHAR(128) NOT NULL,
			PRIMARY KEY (model, account_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}

	for _, ddl := range tables {
		if _, err := d.db.Exec(ddl); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}

	// 自动迁移：添加新字段（忽略已存在的错误）
	alterStmts := []string{
		"ALTER TABLE accounts ADD COLUMN total_success BIGINT DEFAULT 0",
		"ALTER TABLE accounts ADD COLUMN total_429 BIGINT DEFAULT 0",
		"ALTER TABLE accounts ADD COLUMN total_errors BIGINT DEFAULT 0",
	}
	for _, stmt := range alterStmts {
		d.db.Exec(stmt) // 忽略 "Duplicate column" 错误
	}

	return nil
}
