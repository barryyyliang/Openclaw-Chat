package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ChatMessage 表示一条聊天消息
type ChatMessage struct {
	Role      string `json:"role"`      // "user" / "assistant" / "system"
	Content   string `json:"content"`   // 消息正文
	Timestamp string `json:"timestamp"` // ISO8601 时间
}

// ChatHistory 管理某个会话的聊天记录
type ChatHistory struct {
	mu       sync.Mutex
	Messages []ChatMessage `json:"messages"`
	filePath string
}

// NewChatHistory 创建或加载一个会话的聊天历史
func NewChatHistory(sessionID string) *ChatHistory {
	dir, _ := os.UserHomeDir()
	filePath := filepath.Join(dir, ".openclaw-chat-client", "chat_histories", sessionID+".json")

	h := &ChatHistory{
		filePath: filePath,
	}
	h.load()
	return h
}

// Add 添加一条消息并持久化
func (h *ChatHistory) Add(role, content string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	msg := ChatMessage{
		Role:      role,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	h.Messages = append(h.Messages, msg)
	h.save()
}

// GetAll 返回所有历史消息
func (h *ChatHistory) GetAll() []ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.Messages == nil {
		return []ChatMessage{}
	}
	return h.Messages
}

// Clear 清空历史记录并删除文件
func (h *ChatHistory) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.Messages = nil
	os.Remove(h.filePath)
}

// load 从文件加载历史记录
func (h *ChatHistory) load() {
	data, err := os.ReadFile(h.filePath)
	if err != nil {
		h.Messages = nil
		return
	}
	var messages []ChatMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		h.Messages = nil
		return
	}
	h.Messages = messages
}

// save 持久化到文件
func (h *ChatHistory) save() {
	dir := filepath.Dir(h.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Printf("[ChatHistory] 创建目录失败: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(h.Messages, "", "  ")
	if err != nil {
		fmt.Printf("[ChatHistory] 序列化失败: %v\n", err)
		return
	}
	if err := os.WriteFile(h.filePath, data, 0600); err != nil {
		fmt.Printf("[ChatHistory] 写文件失败: %v\n", err)
	}
}
