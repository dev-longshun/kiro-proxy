package kiro

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateRandomMachineID 生成 64 位 hex 格式的 machineId
// 模拟真实 Kiro IDE 行为：machineId 是 SHA256 hash 格式（64 位 hex 字符串）
func GenerateRandomMachineID() string {
	b := make([]byte, 32) // 32 bytes = 64 hex chars
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateMachineID 兼容旧调用
func GenerateMachineID(profileArn, clientID string) string {
	return GenerateRandomMachineID()
}
