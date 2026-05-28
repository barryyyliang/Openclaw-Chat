package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// WailsApp Wails 桌面应用后端
type WailsApp struct {
	ctx      context.Context
	config   *Config
	wsClient *WSClient
	history  *ConnectionHistory

	// 聊天历史
	chatHistory *ChatHistory

	// 流式消息缓冲
	streamBuf   strings.Builder
	streaming   bool
	streamingMu sync.Mutex
}

// NewWailsApp 创建 Wails 应用实例
func NewWailsApp(config *Config) *WailsApp {
	return &WailsApp{
		config:  config,
		history: NewConnectionHistory(),
	}
}

// startup 在 Wails 应用启动时调用
func (a *WailsApp) startup(ctx context.Context) {
	a.ctx = ctx

	// 如果命令行提供了连接参数，自动连接
	if a.hasConnectionParams() {
		go a.connectBackend()
	}
}

// shutdown 在 Wails 应用关闭时调用
func (a *WailsApp) shutdown(ctx context.Context) {
	if a.wsClient != nil {
		a.wsClient.Close()
	}
}

// ─── 暴露给前端的方法 ─────────────────────────────────────────────────────────────

// GetConfig 获取当前配置（前端初始化用）
func (a *WailsApp) GetConfig() map[string]string {
	host, port := a.parseHostPort()
	return map[string]string{
		"host":       host,
		"port":       port,
		"token":      a.config.Token,
		"sessionKey": a.config.SessionKey,
		"deviceId":   a.config.DeviceID,
	}
}

// Connect 连接到服务器
func (a *WailsApp) Connect(host, port, token, sessionKey string) string {
	// 关闭旧连接
	if a.wsClient != nil {
		a.wsClient.Close()
		a.wsClient = nil
	}

	// 更新配置
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5000"
	}

	scheme := "ws"
	a.config.URL = fmt.Sprintf("%s://%s:%s/ws", scheme, host, port)
	a.config.Token = token
	a.config.SessionKey = sessionKey
	a.config.EnsureDeviceIdentity()

	// 连接（保存历史在连接成功后进行）
	a.connectBackend()
	return ""
}

// GetHistory 获取连接历史记录列表
func (a *WailsApp) GetHistory() []ConnectionRecord {
	return a.history.GetAll()
}

// DeleteHistory 删除一条连接历史记录
func (a *WailsApp) DeleteHistory(host, port string) {
	a.history.Delete(host, port)
}

// Disconnect 断开连接
func (a *WailsApp) Disconnect() {
	if a.wsClient != nil {
		a.wsClient.Close()
		a.wsClient = nil
		a.emitEvent("status", map[string]string{"text": "已断开连接"})
	}
}

// GetChatHistory 获取当前会话的聊天历史（前端加载用）
func (a *WailsApp) GetChatHistory(sessionId string) []ChatMessage {
	if sessionId == "" {
		return []ChatMessage{}
	}
	h := NewChatHistory(sessionId)
	return h.GetAll()
}

// ClearChatHistory 清空当前会话的聊天历史
func (a *WailsApp) ClearChatHistory() {
	if a.chatHistory != nil {
		a.chatHistory.Clear()
	}
}

// SendMessage 发送消息
func (a *WailsApp) SendMessage(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "消息不能为空"
	}

	if a.wsClient == nil {
		return "未连接到服务器"
	}

	if err := a.wsClient.SendMessage(text); err != nil {
		return fmt.Sprintf("发送失败: %v", err)
	}

	// 保存用户消息到聊天历史
	if a.chatHistory != nil {
		a.chatHistory.Add("user", text)
	}
	return ""
}

// ─── 内部方法 ─────────────────────────────────────────────────────────────────────

// connectBackend 连接 WebSocket 后端
func (a *WailsApp) connectBackend() {
	a.wsClient = NewWSClient(a.config)

	// 设置回调
	a.wsClient.onStreamDelta = func(delta string) {
		a.streamingMu.Lock()
		if !a.streaming {
			a.streaming = true
			a.streamBuf.Reset()
			// 通知前端开始新的 assistant 消息
			a.emitEvent("stream_start", nil)
		}
		a.streamBuf.WriteString(delta)
		a.streamingMu.Unlock()

		// 发送增量到前端
		a.emitEvent("stream_delta", map[string]string{"delta": delta})
	}

	a.wsClient.onStreamEnd = func() {
		a.streamingMu.Lock()
		a.streaming = false
		fullText := a.streamBuf.String()
		a.streamBuf.Reset()
		a.streamingMu.Unlock()

		// 检测并丢弃记忆插件的内部输出（scene_name/memories/阶段分析等）
		// 这些内容可能因 phase 标记缺失而泄漏到 assistant stream
		if isMemoryPluginOutput(fullText) {
			if a.config.Debug {
				preview := fullText
				if len(preview) > 100 {
					preview = preview[:100] + "..."
				}
				fmt.Printf("  [DEBUG] stream_end: 检测到记忆插件输出，已丢弃: %s\n", preview)
			}
			a.emitEvent("stream_end", nil)
			return
		}

		// 保存 assistant 回复到聊天历史
		if a.chatHistory != nil && fullText != "" {
			a.chatHistory.Add("assistant", fullText)
		}

		a.emitEvent("stream_end", nil)
		a.emitEvent("status", map[string]string{"text": "已连接 — 就绪"})
	}

	a.wsClient.onDisconnect = func(err error) {
		msg := "连接已断开"
		if err != nil {
			msg = fmt.Sprintf("连接断开: %v", err)
		}
		a.emitEvent("status", map[string]string{"text": msg})
		a.emitEvent("disconnected", nil)
	}

	a.wsClient.onToolApproval = func(approvalID, toolName, argsPreview string) {
		a.emitEvent("tool_approval", map[string]string{
			"toolName":    toolName,
			"argsPreview": argsPreview,
		})
		// 自动批准
		if a.wsClient != nil {
			a.wsClient.SendToolApproval(approvalID, true)
		}
	}

	a.wsClient.onMessage = func(frame *ServerFrame) {
		if frame.Error != nil {
			a.emitEvent("error", map[string]string{
				"code":    frame.Error.Code,
				"message": frame.Error.Message,
			})
		}
	}

	// 开始连接
	a.emitEvent("status", map[string]string{"text": "正在连接..."})

	if err := a.wsClient.Connect(); err != nil {
		a.emitEvent("status", map[string]string{"text": fmt.Sprintf("连接失败: %v", err)})
		a.emitEvent("connect_failed", map[string]string{"error": err.Error()})
		return
	}

	// 连接成功
	// sessionKey 用于事件过滤和消息发送，格式为 "agent:<agentId>:<sessionName>"
	// 用户可以只输入 sessionName（如 "woowoo"），会自动补全为 "agent:main:woowoo"
	if a.config.SessionKey != "" {
		a.wsClient.sessionKey = normalizeSessionKey(a.config.SessionKey)
	} else {
		a.wsClient.sessionKey = "agent:main:main"
	}
	a.config.SessionKey = a.wsClient.sessionKey

	// 保存到连接历史（包含 sessionKey）
	host, port := a.parseHostPort()
	a.history.AddOrUpdate(host, port, a.config.Token, a.wsClient.sessionKey)

	// 初始化当前会话的聊天历史
	a.chatHistory = NewChatHistory(a.wsClient.sessionKey)

	a.emitEvent("status", map[string]string{"text": "已连接 — 就绪"})
	a.emitEvent("connected", map[string]string{"sessionKey": a.wsClient.sessionKey})
}

// emitEvent 向前端发送事件
func (a *WailsApp) emitEvent(eventName string, data interface{}) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, eventName, data)
	}
}

// hasConnectionParams 判断是否有有效的连接参数
func (a *WailsApp) hasConnectionParams() bool {
	host, _ := a.parseHostPort()
	if host == "localhost" && a.config.Token == "" && a.config.Password == "" {
		return false
	}
	return true
}

// isMemoryPluginOutput 检测文本是否为 OpenClaw 记忆插件的内部输出
// 这些内容包含场景分析（scene_name）、记忆提取（memories）、阶段分析等
// 它们不应展示在聊天窗口中
func isMemoryPluginOutput(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	// 检测 JSON 格式的记忆输出（以 [ 开头，包含 scene_name 或 memories）
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		if strings.Contains(trimmed, `"scene_name"`) || strings.Contains(trimmed, `"memories"`) || strings.Contains(trimmed, `"record_id"`) {
			return true
		}
	}
	// 检测记忆插件的推理过程文本标记
	if strings.Contains(trimmed, "阶段 0：场景总数检查") ||
		strings.Contains(trimmed, "阶段 1：分析与分类") ||
		strings.Contains(trimmed, "阶段 2：策略选择") ||
		strings.Contains(trimmed, "阶段 3：撰写与合成") {
		return true
	}
	// 检测以 ```json 开头并包含 record_id/action/target_ids 的内容
	if strings.Contains(trimmed, `"action"`) && strings.Contains(trimmed, `"merged_content"`) && strings.Contains(trimmed, `"merged_type"`) {
		return true
	}
	return false
}

// normalizeSessionKey 将用户输入的 sessionKey 规范化为 "agent:<agentId>:<sessionName>" 格式
// 支持的输入：
//   - "woowoo"              → "agent:main:woowoo"   （只有 sessionName）
//   - "myagent:woowoo"      → "agent:myagent:woowoo" （agentId:sessionName）
//   - "agent:main:woowoo"   → "agent:main:woowoo"   （已是完整格式，原样返回）
func normalizeSessionKey(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return "agent:main:main"
	}
	parts := strings.Split(input, ":")
	switch len(parts) {
	case 1:
		// 只有 sessionName，补全为 agent:main:<sessionName>
		return "agent:main:" + parts[0]
	case 2:
		// agentId:sessionName，补全为 agent:<agentId>:<sessionName>
		return "agent:" + parts[0] + ":" + parts[1]
	default:
		// 已经是完整格式或更多段，原样返回
		return input
	}
}

// parseHostPort 从 config.URL 解析 host 和 port
func (a *WailsApp) parseHostPort() (string, string) {
	url := a.config.URL
	url = strings.TrimPrefix(url, "wss://")
	url = strings.TrimPrefix(url, "ws://")

	host := "localhost"
	port := "5000"

	parts := strings.Split(url, ":")
	if len(parts) >= 1 {
		host = parts[0]
	}
	if len(parts) >= 2 {
		portPath := parts[1]
		port = strings.Split(portPath, "/")[0]
	}

	return host, port
}
