package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
	"kiro-proxy/new/proxy"
)

func main() {
	// 全局 panic 捕获 — 写入 crash.log
	defer func() {
		if r := recover(); r != nil {
			crashLog := fmt.Sprintf("[CRASH] %s panic: %v\n", time.Now().Format("2006/01/02 15:04:05"), r)
			// 获取完整的 stack trace
			buf := make([]byte, 1024*64)
			n := runtime.Stack(buf, true)
			crashLog += string(buf[:n])
			os.WriteFile("crash.log", []byte(crashLog), 0644)
			log.Printf("[FATAL] %s", crashLog)
		}
	}()

	// ── Flags ──
	port := flag.Int("port", 8989, "Kiro Proxy 端口")
	accountsFile := flag.String("accounts", "", "Kiro 账号 JSON 文件路径")
	apiKey := flag.String("api-key", "", "API 认证密钥 (也读取 API_KEY 环境变量)")
	logFile := flag.String("log", "kiro-proxy.log", "日志文件路径")
	mysqlDSN := flag.String("mysql", "root:password@tcp(127.0.0.1:3306)/kiro_proxy?charset=utf8mb4&parseTime=true&loc=Local", "MySQL 连接地址")
	debugSave := flag.Bool("debug-save", false, "保存请求/响应到 logs/ 目录")
	enableCache := flag.Bool("cache", true, "启用 prompt caching 模拟")
	redisURL := flag.String("redis", "redis://127.0.0.1:6379", "Redis 连接地址")
	importTest := flag.String("import-test", "", "从 IdC JSON 文件导入并测试账号")
	flag.Parse()

	// ── Debug body save ──
	kiro.SetDebugSave(*debugSave)

	// ── Import test tool ──
	if *importTest != "" {
		proxy.RunImportTest(*importTest)
		return
	}

	// ── Logging ──
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: 无法打开日志文件 %s: %v\n", *logFile, err)
		} else {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
			log.Printf("[Main] 日志输出到文件: %s", *logFile)
		}
	}

	// 重定向 stderr 到 crash.log，捕获 runtime fatal（如 concurrent map write）
	if crashFile, err := os.OpenFile("crash.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		// Linux: 用 Dup2 重定向 fd 2
		if dupErr := redirectStderr(crashFile); dupErr == nil {
			os.Stderr = crashFile
			log.Printf("[Main] stderr 重定向到 crash.log")
		}
	}

	// ══════════════════════════════════════════════
	// Kiro Proxy (端口 8989)
	// ══════════════════════════════════════════════

	db, err := legacy.OpenDatabase(*mysqlDSN)
	if err != nil {
		log.Fatalf("[Main] 数据库打开失败: %v", err)
	}
	db.MigrateFromJSON()

	if *apiKey == "" {
		*apiKey = os.Getenv("API_KEY")
	}

	if *accountsFile == "" {
		entries, _ := os.ReadDir(".")
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" && len(e.Name()) > 13 && e.Name()[:13] == "kiro-accounts" {
				*accountsFile = e.Name()
				break
			}
		}
		if *accountsFile == "" {
			parentEntries, _ := os.ReadDir("..")
			for _, e := range parentEntries {
				if !e.IsDir() && filepath.Ext(e.Name()) == ".json" && len(e.Name()) > 13 && e.Name()[:13] == "kiro-accounts" {
					*accountsFile = filepath.Join("..", e.Name())
					break
				}
			}
		}
	}

	am := proxy.NewAccountManager(*accountsFile)
	dbAccounts, _ := proxy.LoadAccountsFromDB(db)
	if len(dbAccounts) > 0 {
		// 设置状态变化回调（持久化到 DB）
		for _, acc := range dbAccounts {
			acc := acc // capture
			acc.OnStatusChange = func(a *proxy.Account) {
				proxy.SaveAccountToDB(db, a)
			}
		}
		am.Mu.Lock()
		am.Accounts = dbAccounts
		am.Mu.Unlock()
		log.Printf("[Kiro] 从数据库加载了 %d 个账号", len(dbAccounts))
	} else if *accountsFile != "" {
		if err := am.LoadAccounts(); err != nil {
			log.Printf("[Kiro] 从文件加载账号失败: %v", err)
		}
		accounts := am.GetAllAccounts()
		if len(accounts) > 0 {
			proxy.MigrateAccountsFromFile(db, accounts)
		}
	}

	if len(am.GetAllAccounts()) == 0 {
		log.Printf("[Kiro] ⚠️ 未加载任何账号")
	}

	// 为所有账号设置 slot 释放通知
	am.SetupSlotNotify()

	// 加载模型-账号映射
	if modelAccounts, err := db.LoadModelAccounts(); err == nil && len(modelAccounts) > 0 {
		am.ModelAccounts = modelAccounts
		total := 0
		for _, ids := range modelAccounts {
			total += len(ids)
		}
		log.Printf("[Kiro] 加载了 %d 个模型的账号映射（共 %d 条）", len(modelAccounts), total)
		am.SyncModelAccounts()
	}

	am.StartTokenRefreshLoop()

	// Load persisted max concurrent setting
	if v := db.GetSetting("default_max_concurrent"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 100 {
			for _, acc := range am.GetAllAccounts() {
				acc.Mu.Lock()
				acc.MaxConcurrent = n
				acc.Mu.Unlock()
			}
			log.Printf("[Kiro] 从数据库加载并发限制: max_concurrent=%d", n)
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Main] panic in session cleanup: %v", r)
			}
		}()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			am.CleanStaleSessions()
		}
	}()
	db.StartPeriodicSync(func() error {
		return proxy.SaveAllAccountsToDB(db, am.GetAllAccounts())
	}, 2*time.Minute)

	usageTracker := legacy.NewUsageTracker(db)
	usageTracker.StartPeriodicSave()
	proxyMgr := legacy.NewProxyManager(db)
	keyMgr := legacy.NewKeyManager(db)
	legacy.GlobalRateLimiter.LoadConfigFromDB(db)

	// Load model strip patterns from DB
	if v := db.GetSetting("model_strip_patterns"); v != "" {
		var patterns []string
		for _, p := range strings.Split(v, "\n") {
			p = strings.TrimSpace(p)
			if p != "" {
				patterns = append(patterns, p)
			}
		}
		kiro.ModelStripPatterns = patterns
		log.Printf("[Models] 加载了 %d 个模型名清理规则", len(patterns))
	}

	// Load custom Kiro models from DB
	if v := db.GetSetting("kiro_models"); v != "" {
		var models []string
		for _, m := range strings.Split(v, "\n") {
			m = strings.TrimSpace(m)
			if m != "" {
				models = append(models, m)
			}
		}
		if len(models) > 0 {
			kiro.KiroModels = models
			log.Printf("[Models] 加载了 %d 个自定义模型", len(models))
		}
	}

	// Load model aliases from DB
	if v := db.GetSetting("model_aliases"); v != "" {
		aliases := make(map[string]string)
		for _, line := range strings.Split(v, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.Contains(line, "=") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			from := strings.TrimSpace(parts[0])
			to := strings.TrimSpace(parts[1])
			if from != "" && to != "" {
				aliases[strings.ToLower(from)] = to
			}
		}
		kiro.ModelAliases = aliases
		log.Printf("[Models] 加载了 %d 个模型映射", len(aliases))
	}

	proxyMgr.ReconcileBindings(toAccountLike(am.GetAllAccounts()))
	am.SetProxyManager(proxyMgr)

	kiroServer := proxy.NewServer(am, *port, *apiKey, usageTracker, proxyMgr, keyMgr, legacy.GlobalRateLimiter)
	kiroServer.DB = db
	kiroServer.EnableCache = *enableCache
	log.Printf("[Main] Prompt caching: %v", *enableCache)

	// 初始化缓存后端
	if *redisURL != "" {
		redisStore, err := proxy.NewRedisCacheStore(*redisURL)
		if err != nil {
			log.Printf("[Main] Redis 连接失败，降级到内存缓存: %v", err)
		} else {
			kiroServer.SetCacheStore(redisStore)
		}
	} else {
		log.Printf("[Main] 缓存后端: 内存")
	}
	kiroServer.StartUsageLimitsLoop()

	// 定期清理过期 session
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Main] panic in session tracker cleanup: %v", r)
			}
		}()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			kiroServer.CleanupSessions()
		}
	}()

	// ── Start server ──
	srv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", *port),
		Handler: kiroServer.Handler(),
	}

	printBanner(*port, len(am.GetAllAccounts()))

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Main] 服务启动失败: %v", err)
		}
	}()

	// ── Graceful shutdown ──
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("[Main] 正在关闭服务...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	db.Shutdown(func() error {
		return proxy.SaveAllAccountsToDB(db, am.GetAllAccounts())
	})
	log.Println("[Main] 已退出")
}

func printBanner(port, accounts int) {
	fmt.Printf(`
+--------------------------------------------------------------+
|                    Kiro API Proxy                            |
+--------------------------------------------------------------+
|  Kiro Proxy:  http://127.0.0.1:%-5d                         |
+--------------------------------------------------------------+
|  Kiro 账号: %-3d                                              |
+--------------------------------------------------------------+
`, port, accounts)
}

func toAccountLike(accounts []*proxy.Account) []legacy.AccountLike {
	result := make([]legacy.AccountLike, len(accounts))
	for i, a := range accounts {
		result[i] = a
	}
	return result
}
