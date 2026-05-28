package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 保存连接配置
type Config struct {
	URL        string // WebSocket URL, e.g. ws://localhost:5000/ws
	Token      string // Bearer token 认证
	Password   string // Password 认证
	SessionKey string // 会话 Key (如 "agent:main:main")
	ClientID   string // 客户端唯一标识（必须是 OpenClaw 允许的枚举值）
	Debug bool // 调试模式，输出详细日志

	// Ed25519 设备身份
	PrivateKey ed25519.PrivateKey // Ed25519 私钥（64字节）
	PublicKey  ed25519.PublicKey  // Ed25519 公钥（32字节）
	DeviceID   string             // 设备 ID = SHA256(publicKey).hex()

	// 密钥文件路径（用于持久化）
	KeyFile string
}

// DeviceIdentity 持久化存储的设备身份
type DeviceIdentity struct {
	PrivateKey string `json:"privateKey"` // base64url 编码的 Ed25519 私钥种子（32字节）
	PublicKey  string `json:"publicKey"`  // base64url 编码的 Ed25519 公钥（32字节）
	DeviceID   string `json:"deviceId"`   // SHA256(publicKey).hex()
}

// GenerateClientID 生成随机客户端 ID（已废弃，使用固定枚举值）
func GenerateClientID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "chat_" + hex.EncodeToString(b)
}

// EnsureDeviceIdentity 确保设备身份存在（如果不存在则生成新的）
func (c *Config) EnsureDeviceIdentity() error {
	// 确定密钥文件路径
	if c.KeyFile == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			homeDir = "."
		}
		c.KeyFile = filepath.Join(homeDir, ".openclaw-chat-client", "device.json")
	}

	// 尝试加载已有密钥
	if err := c.loadDeviceIdentity(); err == nil {
		return nil // 已有密钥，直接使用
	}

	// 生成新的 Ed25519 密钥对
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 Ed25519 密钥对失败: %w", err)
	}

	c.PrivateKey = priv
	c.PublicKey = pub
	c.DeviceID = deriveDeviceID(pub)

	// 持久化存储
	return c.saveDeviceIdentity()
}

// loadDeviceIdentity 从文件加载设备身份
func (c *Config) loadDeviceIdentity() error {
	data, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return err
	}

	var identity DeviceIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return err
	}

	// 解码私钥种子
	seed, err := base64UrlDecode(identity.PrivateKey)
	if err != nil {
		return fmt.Errorf("解码私钥失败: %w", err)
	}

	// 从种子重建完整私钥
	c.PrivateKey = ed25519.NewKeyFromSeed(seed)
	c.PublicKey = c.PrivateKey.Public().(ed25519.PublicKey)
	c.DeviceID = identity.DeviceID

	// 验证 deviceID 一致性
	expectedID := deriveDeviceID(c.PublicKey)
	if expectedID != c.DeviceID {
		return fmt.Errorf("设备 ID 不匹配: 期望 %s, 实际 %s", expectedID, c.DeviceID)
	}

	return nil
}

// saveDeviceIdentity 将设备身份存储到文件
func (c *Config) saveDeviceIdentity() error {
	// 创建目录
	dir := filepath.Dir(c.KeyFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("创建密钥目录失败: %w", err)
	}

	identity := DeviceIdentity{
		PrivateKey: base64UrlEncode(c.PrivateKey.Seed()),
		PublicKey:  base64UrlEncode([]byte(c.PublicKey)),
		DeviceID:   c.DeviceID,
	}

	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(c.KeyFile, data, 0600)
}

// deriveDeviceID 从公钥派生设备 ID: SHA256(raw_32byte_public_key).hex()
func deriveDeviceID(pub ed25519.PublicKey) string {
	hash := sha256.Sum256([]byte(pub))
	return hex.EncodeToString(hash[:])
}

// base64UrlEncode 将字节编码为 base64url（无填充）
func base64UrlEncode(data []byte) string {
	s := base64.StdEncoding.EncodeToString(data)
	s = strings.ReplaceAll(s, "+", "-")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.TrimRight(s, "=")
	return s
}

// base64UrlDecode 解码 base64url 字符串
func base64UrlDecode(s string) ([]byte, error) {
	// 还原标准 base64
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	// 补齐填充
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.StdEncoding.DecodeString(s)
}

// SignDevicePayload 对 payload 进行 Ed25519 签名，返回 base64url 编码的签名
func (c *Config) SignDevicePayload(payload string) string {
	sig := ed25519.Sign(c.PrivateKey, []byte(payload))
	return base64UrlEncode(sig)
}

// BuildDeviceAuthPayloadV2 构建 v2 版本的签名 payload
// 格式: v2|deviceId|clientId|clientMode|role|scopes|signedAtMs|token|nonce
func BuildDeviceAuthPayloadV2(deviceID, clientID, clientMode, role string, scopes []string, signedAtMs int64, token, nonce string) string {
	scopesStr := strings.Join(scopes, ",")
	if token == "" {
		// token 为空时保持空字符串
	}
	return strings.Join([]string{
		"v2",
		deviceID,
		clientID,
		clientMode,
		role,
		scopesStr,
		fmt.Sprintf("%d", signedAtMs),
		token,
		nonce,
	}, "|")
}
