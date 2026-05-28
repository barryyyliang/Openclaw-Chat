package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ConnectionRecord 单条连接历史记录
type ConnectionRecord struct {
	Host       string `json:"host"`
	Port       string `json:"port"`
	Token      string `json:"token"`
	SessionKey string `json:"sessionKey"` // 会话 Key，用于恢复到同一会话
	Label      string `json:"label"`      // 可选的用户自定义标签
	LastUsed   string `json:"lastUsed"`   // ISO8601 时间戳
	UsedCount  int    `json:"usedCount"`  // 使用次数
}

// ConnectionHistory 连接历史管理
type ConnectionHistory struct {
	Records  []ConnectionRecord `json:"records"`
	filePath string
}

// NewConnectionHistory 创建历史管理实例
func NewConnectionHistory() *ConnectionHistory {
	h := &ConnectionHistory{}
	h.filePath = h.getFilePath()
	h.load()
	return h
}

// getFilePath 获取历史文件路径
func (h *ConnectionHistory) getFilePath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "."
	}
	return filepath.Join(homeDir, ".openclaw-chat-client", "connection_history.json")
}

// load 从文件加载历史记录
func (h *ConnectionHistory) load() {
	data, err := os.ReadFile(h.filePath)
	if err != nil {
		h.Records = []ConnectionRecord{}
		return
	}
	if err := json.Unmarshal(data, &h.Records); err != nil {
		h.Records = []ConnectionRecord{}
	}
}

// save 保存历史记录到文件
func (h *ConnectionHistory) save() error {
	dir := filepath.Dir(h.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h.Records, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(h.filePath, data, 0600)
}

// AddOrUpdate 添加或更新一条记录（基于 host+port 去重）
func (h *ConnectionHistory) AddOrUpdate(host, port, token, sessionKey string) {
	now := time.Now().Format(time.RFC3339)

	for i, r := range h.Records {
		if r.Host == host && r.Port == port {
			// 已存在，更新 token、sessionKey 和时间
			h.Records[i].Token = token
			if sessionKey != "" {
				h.Records[i].SessionKey = sessionKey
			}
			h.Records[i].LastUsed = now
			h.Records[i].UsedCount++
			// 移到第一个位置（最近使用优先）
			record := h.Records[i]
			h.Records = append(h.Records[:i], h.Records[i+1:]...)
			h.Records = append([]ConnectionRecord{record}, h.Records...)
			h.save()
			return
		}
	}

	// 新记录，插入到最前面
	record := ConnectionRecord{
		Host:       host,
		Port:       port,
		Token:      token,
		SessionKey: sessionKey,
		LastUsed:   now,
		UsedCount:  1,
	}
	h.Records = append([]ConnectionRecord{record}, h.Records...)

	// 最多保留 20 条
	if len(h.Records) > 20 {
		h.Records = h.Records[:20]
	}

	h.save()
}

// Delete 删除一条记录
func (h *ConnectionHistory) Delete(host, port string) {
	for i, r := range h.Records {
		if r.Host == host && r.Port == port {
			h.Records = append(h.Records[:i], h.Records[i+1:]...)
			h.save()
			return
		}
	}
}

// GetAll 获取所有记录
func (h *ConnectionHistory) GetAll() []ConnectionRecord {
	return h.Records
}
