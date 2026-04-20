package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/legacy"
)

// idcEntry represents one entry in results5.json (IdC format)
type idcEntry struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// RunImportTest reads an IdC JSON file, tests each account (refresh, usage, chat), and generates import files.
func RunImportTest(filePath string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("读取文件失败 %s: %v", filePath, err)
	}

	var entries []idcEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Fatalf("解析JSON失败: %v", err)
	}

	fmt.Printf("📂 从 %s 加载了 %d 个账号\n\n", filePath, len(entries))

	// ── Step 1: Token 刷新测试 ──
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("  Step 1: Token 刷新测试")
	fmt.Println("═══════════════════════════════════════")

	var validAccounts []*Account
	for i, e := range entries {
		shortID := e.ClientID
		if len(shortID) > 16 {
			shortID = shortID[:16]
		}
		fmt.Printf("\n[%d/%d] ClientID: %s...\n", i+1, len(entries), shortID)

		creds := &kiro.KiroCredentials{
			AccessToken:  e.AccessToken,
			RefreshToken: e.RefreshToken,
			ClientID:     e.ClientID,
			ClientSecret: e.ClientSecret,
			Region:       "us-east-1",
			AuthMethod:   "idc",
		}

		acc := &Account{
			ID:          fmt.Sprintf("idc_%d", i+1),
			Email:       fmt.Sprintf("idc-account-%d@import", i+1),
			Nickname:    fmt.Sprintf("IdC #%d", i+1),
			MachineID:   kiro.GenerateRandomMachineID(),
			Credentials: creds,
			Status:      kiro.StatusActive,
			Enabled:     true,
		}

		refresher := &kiro.TokenRefresher{Creds: creds}
		ok, result := refresher.Refresh()
		if ok {
			tokenPreview := creds.AccessToken
			if len(tokenPreview) > 20 {
				tokenPreview = tokenPreview[:20]
			}
			fmt.Printf("  ✅ Token刷新成功! 新Token: %s...\n", tokenPreview)
			validAccounts = append(validAccounts, acc)
		} else {
			fmt.Printf("  ❌ Token刷新失败: %s\n", kiro.TruncStr(result, 200))
			if creds.AccessToken != "" {
				fmt.Printf("  ⚠️ 使用现有Token继续\n")
				validAccounts = append(validAccounts, acc)
			}
		}
	}

	fmt.Printf("\n📊 刷新结果: %d/%d 个账号可用\n", len(validAccounts), len(entries))
	if len(validAccounts) == 0 {
		fmt.Println("\n❌ 没有可用账号，退出")
		os.Exit(1)
	}
	// ── Step 2: 查询用量 ──
	fmt.Println("\n═══════════════════════════════════════")
	fmt.Println("  Step 2: 查询 Kiro 用量")
	fmt.Println("═══════════════════════════════════════")

	client := kiro.NewKiroClient()
	for i, acc := range validAccounts {
		fmt.Printf("\n[%d] %s\n", i+1, acc.Email)
		token := acc.GetToken()
		machineID := acc.GetMachineID()

		limits, err := client.GetUsageLimits(token, machineID)
		if err != nil {
			fmt.Printf("  ❌ 查询用量失败: %s\n", kiro.TruncStr(err.Error(), 200))
		} else {
			fmt.Printf("  📊 订阅: %s\n", limits.SubscriptionTitle)
			pct := 0.0
			if limits.UsageLimit > 0 {
				pct = (limits.CurrentUsage / limits.UsageLimit) * 100
			}
			fmt.Printf("  📊 用量: %.2f / %.2f (%.1f%%)\n", limits.CurrentUsage, limits.UsageLimit, pct)
			if limits.FreeTrialLimit > 0 {
				fmt.Printf("  🆓 免费试用: %.2f / %.2f\n", limits.FreeTrialUsage, limits.FreeTrialLimit)
			}
			fmt.Printf("  📅 重置: %d 天后\n", limits.DaysUntilReset)
		}
	}
	// ── Step 3: 发送测试请求 ──
	fmt.Println("\n═══════════════════════════════════════")
	fmt.Println("  Step 3: 发送 Kiro 聊天请求测试")
	fmt.Println("═══════════════════════════════════════")

	var chatSuccess bool
	for idx, testAcc := range validAccounts {
		fmt.Printf("\n尝试账号 [%d]: %s\n", idx+1, testAcc.Email)
		token := testAcc.GetToken()
		machineID := testAcc.GetMachineID()

		startTime := time.Now()
		result, statusCode, err := client.SendRequest(
			token, machineID,
			"claude-sonnet-4",
			"Say hello in one word",
			nil, nil, nil, nil, "",
		)
		duration := time.Since(startTime)

		if err != nil {
			fmt.Printf("  ❌ 请求失败 (HTTP %d): %s\n", statusCode, kiro.TruncStr(err.Error(), 200))
			if statusCode == 429 {
				fmt.Println("  ⚠️ 被限流(429)，账号正常，尝试下一个...")
				continue
			}
			continue
		}

		content := strings.Join(result.Content, "")
		fmt.Printf("  ✅ 请求成功! (耗时 %v)\n", duration.Round(time.Millisecond))
		fmt.Printf("  📝 回复: %s\n", content)
		fmt.Printf("  🔄 StopReason: %s\n", result.StopReason)
		if result.CreditsUsed > 0 {
			fmt.Printf("  💰 Credits: %.4f\n", result.CreditsUsed)
		}
		chatSuccess = true
		break
	}

	if !chatSuccess {
		fmt.Println("\n⚠️ 所有账号都无法完成聊天请求（可能全部被限流），但账号本身已验证可用")
	}
	// ── Step 4: 生成导入文件 ──
	fmt.Println("\n═══════════════════════════════════════")
	fmt.Println("  Step 4: 生成 Kiro 账号文件")
	fmt.Println("═══════════════════════════════════════")

	var jsonAccounts []kiro.AccountJSON
	for _, acc := range validAccounts {
		jsonAccounts = append(jsonAccounts, kiro.AccountJSON{
			ID:       acc.ID,
			Email:    acc.Email,
			Nickname: acc.Nickname,
			Credentials: kiro.KiroCredentials{
				AccessToken:  acc.Credentials.AccessToken,
				RefreshToken: acc.Credentials.RefreshToken,
				ClientID:     acc.Credentials.ClientID,
				ClientSecret: acc.Credentials.ClientSecret,
				Region:       "us-east-1",
				AuthMethod:   "idc",
			},
			Status: "active",
		})
	}

	af := &kiro.AccountsFile{
		Version:    "1.0",
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   jsonAccounts,
	}

	outFile := "kiro-accounts-imported.json"
	outData, _ := json.MarshalIndent(af, "", "  ")
	os.WriteFile(outFile, outData, 0644)

	fmt.Printf("\n✅ 已生成 %s (%d 个账号)\n", outFile, len(jsonAccounts))
	fmt.Println("\n使用方式:")
	fmt.Printf("  go run . --accounts %s\n", outFile)
	fmt.Println("\n或直接启动服务（会自动从数据库加载）:")
	fmt.Println("  go run .")

	// ── Step 5: 写入数据库 ──
	fmt.Println("\n═══════════════════════════════════════")
	fmt.Println("  Step 5: 写入数据库")
	fmt.Println("═══════════════════════════════════════")

	db, err := legacy.OpenDatabase("kiro-proxy.db")
	if err != nil {
		fmt.Printf("  ❌ 打开数据库失败: %v\n", err)
		return
	}

	saved := 0
	for _, acc := range validAccounts {
		if err := SaveAccountToDB(db, acc); err != nil {
			fmt.Printf("  ❌ 保存 %s 失败: %v\n", acc.Email, err)
		} else {
			saved++
		}
	}
	fmt.Printf("\n✅ 已写入数据库 %d/%d 个账号\n", saved, len(validAccounts))
	fmt.Println("  下次启动服务时会自动从数据库加载这些账号")
}
