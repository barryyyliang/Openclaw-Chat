package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// generateIdempotencyKey 生成唯一的幂等键（16字节随机 hex）
func generateIdempotencyKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}



// WSClient WebSocket 客户端
type WSClient struct {
	config *Config
	conn   *websocket.Conn
	mu     sync.Mutex

	// 回调函数
	onMessage      func(frame *ServerFrame)
	onDisconnect   func(err error)
	onStreamDelta  func(delta string)
	onStreamEnd    func()
	onToolApproval func(approvalID, toolName, argsPreview string)

	ctx    context.Context
	cancel context.CancelFunc

	// 调试模式
	debug bool

	// 握手状态
	connected    bool              // 是否完成 hello-ok 握手
	connectCh    chan error        // 握手完成通知
	challengeCh  chan *ChallengePayload // 等待 challenge 通知
	connID       string            // 服务器分配的连接 ID（来自 hello-ok 握手）
	sessionKey   string            // 当前使用的 Session Key（如 "agent:main:main"，用于事件过滤）

	// 请求 ID 计数器
	reqIDCounter uint64
}

// NewWSClient 创建新的 WebSocket 客户端
func NewWSClient(config *Config) *WSClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &WSClient{
		config:      config,
		ctx:         ctx,
		cancel:      cancel,
		debug:       config.Debug,
		connectCh:   make(chan error, 1),
		challengeCh: make(chan *ChallengePayload, 1),
	}
}

// Connect 连接到 OpenClaw Gateway 并完成握手
func (c *WSClient) Connect() error {
	header := http.Header{}

	// 注意：认证通过 connect 帧中的 auth 字段传递，不再用 HTTP header
	// 但某些部署可能同时支持 header 认证，保留兼容
	if c.config.Token != "" {
		header.Set("Authorization", "Bearer "+c.config.Token)
	}

	// 设置 WebSocket 连接选项
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	if c.debug {
		fmt.Printf("  [DEBUG] 正在连接: %s\n", c.config.URL)
	}

	conn, resp, err := dialer.DialContext(c.ctx, c.config.URL, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("WebSocket 连接失败 (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	c.conn = conn

	// 设置连接参数
	conn.SetReadDeadline(time.Time{}) // 无超时

	// Ping 处理器
	conn.SetPingHandler(func(appData string) error {
		if c.debug {
			fmt.Printf("  [DEBUG] 收到 Ping, 回复 Pong\n")
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	// Pong 处理器
	conn.SetPongHandler(func(appData string) error {
		if c.debug {
			fmt.Printf("  [DEBUG] 收到 Pong 回复\n")
		}
		return nil
	})

	// Close 处理器
	conn.SetCloseHandler(func(code int, text string) error {
		if c.debug {
			fmt.Printf("  [DEBUG] 收到 Close 帧: code=%d, reason=%q\n", code, text)
		}
		message := websocket.FormatCloseMessage(code, "")
		c.conn.WriteControl(websocket.CloseMessage, message, time.Now().Add(time.Second))
		return nil
	})

	// 启动消息接收协程
	go c.readLoop()

	// 第1步：等待 connect.challenge 事件（超时 15 秒）
	if c.debug {
		fmt.Printf("  [DEBUG] 等待 connect.challenge...\n")
	}
	var challenge *ChallengePayload
	select {
	case challenge = <-c.challengeCh:
		if c.debug {
			fmt.Printf("  [DEBUG] 收到 challenge: nonce=%s, ts=%d\n", challenge.Nonce, challenge.Ts)
		}
	case err := <-c.connectCh:
		// 握手阶段就收到了错误（如服务器直接关闭连接）
		c.conn.Close()
		if err != nil {
			return fmt.Errorf("握手失败(等待challenge): %w", err)
		}
		return fmt.Errorf("连接在等待challenge时被关闭")
	case <-time.After(15 * time.Second):
		c.conn.Close()
		return fmt.Errorf("等待 connect.challenge 超时(15秒)")
	case <-c.ctx.Done():
		return fmt.Errorf("连接已取消")
	}

	// 第2步：发送 Connect 请求帧（带 challenge nonce）
	if err := c.sendConnectFrame(challenge); err != nil {
		c.conn.Close()
		return fmt.Errorf("发送握手帧失败: %w", err)
	}

	// 第3步：等待 hello-ok 响应（超时 10 秒）
	select {
	case err := <-c.connectCh:
		if err != nil {
			c.conn.Close()
			return fmt.Errorf("握手失败: %w", err)
		}
	case <-time.After(10 * time.Second):
		c.conn.Close()
		return fmt.Errorf("握手超时: 10秒内未收到 hello-ok")
	case <-c.ctx.Done():
		return fmt.Errorf("连接已取消")
	}

	if c.debug {
		fmt.Printf("  [DEBUG] 握手成功! connID=%s, sessionKey=%s\n", c.connID, c.sessionKey)
	}

	// 启动心跳
	go c.pingLoop()

	return nil
}

// sendConnectFrame 发送 Connect 握手帧（正确的 req 帧格式 + Ed25519 签名）
func (c *WSClient) sendConnectFrame(challenge *ChallengePayload) error {
	// 使用 OpenClaw 允许的 client.id 和 client.mode 枚举值
	clientID := c.config.ClientID  // 应为 "cli" 或 "gateway-client"
	clientMode := "ui"             // 枚举: cli, ui, webchat, backend, node, probe, test
	role := "operator"
	scopes := []string{"operator.read", "operator.write"}
	signedAtMs := time.Now().UnixMilli()

	params := ConnectParams{
		MinProtocol: 3,
		MaxProtocol: ProtocolVersion,
		Client: ClientInfo{
			ID:       clientID,
			Version:  "1.0.0",
			Platform: "macos",
			Mode:     clientMode,
		},
		Role:      role,
		Scopes:    scopes,
		Locale:    "zh-CN",
		UserAgent: "openclaw-chat-client/1.0.0",
	}

	// 设置认证信息
	token := ""
	if c.config.Token != "" {
		params.Auth = &ConnectAuth{
			Token: c.config.Token,
		}
		token = c.config.Token
	} else if c.config.Password != "" {
		params.Auth = &ConnectAuth{
			Password: c.config.Password,
		}
	}

	// 构建签名 payload (v2 格式): v2|deviceId|clientId|clientMode|role|scopes|signedAtMs|token|nonce
	payload := BuildDeviceAuthPayloadV2(
		c.config.DeviceID,
		clientID,
		clientMode,
		role,
		scopes,
		signedAtMs,
		token,
		challenge.Nonce,
	)

	if c.debug {
		fmt.Printf("  [DEBUG] 签名 payload: %s\n", payload)
	}

	// 用 Ed25519 私钥签名
	signature := c.config.SignDevicePayload(payload)

	// 设置设备身份信息（含签名）
	params.Device = &DeviceInfo{
		ID:        c.config.DeviceID,
		PublicKey: base64UrlEncode([]byte(c.config.PublicKey)),
		Signature: signature,
		SignedAt:  signedAtMs,
		Nonce:    challenge.Nonce,
	}

	req := ConnectRequest{
		Type:   "req",
		ID:     c.nextReqID(),
		Method: "connect",
		Params: params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	if c.debug {
		fmt.Printf("  [DEBUG] 发送 Connect 请求帧: %s\n", string(data))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// nextReqID 生成下一个请求 ID
func (c *WSClient) nextReqID() string {
	id := atomic.AddUint64(&c.reqIDCounter, 1)
	return fmt.Sprintf("req_%d", id)
}

// SendMessage 发送用户消息（通过 RPC 方法 chat.send）
func (c *WSClient) SendMessage(text string) error {
	if !c.connected {
		return fmt.Errorf("尚未完成握手，无法发送消息")
	}

	// RPC 模式: 发送 RequestFrame
	// sessionKey 格式: "agent:<agentId>:<sessionName>"
	sessionKey := c.sessionKey
	if sessionKey == "" {
		sessionKey = "agent:main:main" // 默认主会话
	}

	// 生成幂等键（UUID 格式）
	idempotencyKey := generateIdempotencyKey()

	params := ChatSendParams{
		SessionKey:     sessionKey,
		Message:        text,
		IdempotencyKey: idempotencyKey,
	}

	req := RequestFrame{
		Type:   "req",
		ID:     c.nextReqID(),
		Method: MethodChatSend,
		Params: params,
	}

	if c.debug {
		fmt.Printf("  [DEBUG] chat.send sessionKey=%s, idempotencyKey=%s\n", sessionKey, idempotencyKey)
	}

	return c.sendJSON(&req)
}

// SendToolApproval 发送工具审批决定
func (c *WSClient) SendToolApproval(approvalID string, approved bool) error {
	req := RequestFrame{
		Type:   "req",
		ID:     c.nextReqID(),
		Method: "tool.approve",
		Params: map[string]interface{}{
			"approvalId": approvalID,
			"approved":   approved,
		},
	}
	return c.sendJSON(&req)
}

// Close 关闭连接
func (c *WSClient) Close() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
		c.conn.Close()
	}
}

// sendJSON 发送 JSON 消息
func (c *WSClient) sendJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("连接未建立")
	}

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}

	if c.debug {
		fmt.Printf("  [DEBUG] 发送: %s\n", string(data))
	}

	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// pingLoop 定期发送 Ping 帧保持连接
func (c *WSClient) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.conn != nil {
				err := c.conn.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(5*time.Second))
				if err != nil && c.debug {
					fmt.Printf("  [DEBUG] 发送 Ping 失败: %v\n", err)
				}
			}
			c.mu.Unlock()
		}
	}
}

// readLoop 消息接收循环
func (c *WSClient) readLoop() {
	defer func() {
		if c.onDisconnect != nil {
			c.onDisconnect(nil)
		}
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msgType, message, err := c.conn.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				closeReason := fmt.Errorf("服务器关闭连接: code=%d (%s), reason=%q",
					closeErr.Code, closeCodeToString(closeErr.Code), closeErr.Text)
				if c.debug {
					fmt.Printf("  [DEBUG] %v\n", closeReason)
				}
				if !c.connected {
					// 握手阶段断开
					c.connectCh <- closeReason
				}
				if c.onDisconnect != nil {
					c.onDisconnect(closeReason)
				}
			} else if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				if !c.connected {
					c.connectCh <- err
				}
				if c.onDisconnect != nil {
					c.onDisconnect(fmt.Errorf("连接异常断开: %w", err))
				}
			}
			return
		}

		// 只处理文本消息
		if msgType == websocket.TextMessage {
			if c.debug {
				fmt.Printf("  [DEBUG] 收到: %s\n", string(message))
			}
			c.handleFrame(message)
		}
	}
}

// handleFrame 处理收到的帧
func (c *WSClient) handleFrame(data []byte) {
	frame, err := parseServerFrame(data)
	if err != nil {
		if c.debug {
			fmt.Printf("  [DEBUG] 帧解析失败: %v, 原始数据: %s\n", err, string(data))
		}
		// 不是 JSON，按纯文本处理
		if c.onMessage != nil {
			c.onMessage(&ServerFrame{
				Event: EventAssistantMessage,
				Data:  data,
			})
		}
		return
	}

	// 握手阶段：优先处理 connect.challenge 和 hello-ok
	if !c.connected {
		// 检查是否是 connect.challenge 事件
		if frame.Type == "event" && frame.Event == "connect.challenge" {
			c.handleChallenge(frame)
			return
		}

		// 检查是否是 hello-ok 响应（格式：type=res, ok=true, payload.type=hello-ok）
		if frame.Type == "res" || (frame.ID != nil && frame.OK != nil) {
			c.handleConnectResponse(frame)
			return
		}

		// 兼容旧格式 hello-ok（直接 type=hello-ok）
		if frame.Type == TypeHelloOK {
			c.handleTypedFrame(frame)
			return
		}
	}

	// 处理带 type 字段的帧（非握手阶段）
	if frame.IsTyped() && frame.Type != "event" && frame.Type != "req" && frame.Type != "res" {
		c.handleTypedFrame(frame)
		return
	}

	// 处理 Response 帧
	if frame.IsResponse() {
		c.handleResponseFrame(frame)
		return
	}

	// 处理 Event 帧
	if frame.IsEvent() {
		c.handleEventFrame(frame)
		return
	}

	// 处理 type=event 但 event 字段为空的情况（payload 在外层）
	if frame.Type == "event" && frame.Event == "" {
		// 可能 event 在 payload 中
		if c.debug {
			fmt.Printf("  [DEBUG] type=event 但无 event 字段\n")
		}
	}

	// 未知帧类型，直接回调
	if c.onMessage != nil {
		c.onMessage(frame)
	}
}

// handleChallenge 处理 connect.challenge 事件
func (c *WSClient) handleChallenge(frame *ServerFrame) {
	var challenge ChallengePayload

	// challenge 数据可能在 payload 或 data 字段
	raw := frame.Payload
	if raw == nil {
		raw = frame.Data
	}

	if raw != nil {
		if err := json.Unmarshal(raw, &challenge); err != nil {
			if c.debug {
				fmt.Printf("  [DEBUG] 解析 challenge payload 失败: %v\n", err)
			}
			return
		}
	}

	// 发送 challenge 到等待的协程
	select {
	case c.challengeCh <- &challenge:
	default:
		// channel 已满，忽略重复的 challenge
	}
}

// handleConnectResponse 处理 connect 方法的响应（hello-ok）
func (c *WSClient) handleConnectResponse(frame *ServerFrame) {
	if frame.OK != nil && *frame.OK {
		// 握手成功！解析 payload 获取 sessionId 等
		if frame.Payload != nil {
			var payload struct {
				Type     string `json:"type"`
				Protocol int    `json:"protocol"`
				Server   struct {
					Version string `json:"version"`
					ConnID  string `json:"connId"`
				} `json:"server"`
				Auth struct {
					DeviceToken string `json:"deviceToken"`
					Role        string `json:"role"`
				} `json:"auth"`
				Policy struct {
					MaxPayload       int `json:"maxPayload"`
					TickIntervalMs   int `json:"tickIntervalMs"`
				} `json:"policy"`
			}
			if err := json.Unmarshal(frame.Payload, &payload); err == nil {
				if payload.Server.ConnID != "" {
					c.connID = payload.Server.ConnID
				}
				if c.debug {
					fmt.Printf("  [DEBUG] hello-ok: protocol=%d, connId=%s\n", payload.Protocol, payload.Server.ConnID)
				}
			}
		}
		// 也尝试从 Result 字段解析（某些版本可能用 result 代替 payload）
		if c.connID == "" && frame.Result != nil {
			var result struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionId"`
				Server    struct {
					ConnID string `json:"connId"`
				} `json:"server"`
			}
			if err := json.Unmarshal(frame.Result, &result); err == nil {
				if result.SessionID != "" {
					c.connID = result.SessionID
				} else if result.Server.ConnID != "" {
					c.connID = result.Server.ConnID
				}
			}
		}
		c.connected = true
		c.connectCh <- nil
	} else {
		// 握手失败
		errMsg := "认证失败"
		if frame.Error != nil {
			errMsg = fmt.Sprintf("[%s] %s", frame.Error.Code, frame.Error.Message)
		}
		c.connectCh <- fmt.Errorf("%s", errMsg)
	}
}

// handleTypedFrame 处理带 type 字段的帧（hello-ok, unauthorized 等）
func (c *WSClient) handleTypedFrame(frame *ServerFrame) {
	switch frame.Type {
	case TypeHelloOK:
		// 握手成功
		if frame.Payload != nil {
			var payload HelloOKPayload
			if err := json.Unmarshal(frame.Payload, &payload); err == nil {
				if payload.SessionID != "" {
					c.connID = payload.SessionID
				}
			}
		}
		c.connected = true
		c.connectCh <- nil

	case TypeUnauthorized:
		c.connectCh <- fmt.Errorf("认证失败: unauthorized")

	default:
		if c.debug {
			fmt.Printf("  [DEBUG] 未知类型帧: type=%s\n", frame.Type)
		}
		if c.onMessage != nil {
			c.onMessage(frame)
		}
	}
}

// handleResponseFrame 处理 RPC 响应帧
func (c *WSClient) handleResponseFrame(frame *ServerFrame) {
	if frame.OK != nil && !*frame.OK {
		// RPC 调用失败
		errMsg := "未知错误"
		if frame.Error != nil {
			errMsg = fmt.Sprintf("[%s] %s", frame.Error.Code, frame.Error.Message)
		}
		if c.debug {
			fmt.Printf("  [DEBUG] RPC 错误: %s\n", errMsg)
		}
		if c.onMessage != nil {
			c.onMessage(frame)
		}
		return
	}

	// RPC 成功响应
	if c.onMessage != nil {
		c.onMessage(frame)
	}
}

// handleEventFrame 处理事件帧
func (c *WSClient) handleEventFrame(frame *ServerFrame) {
	switch frame.Event {

	// ─── OpenClaw "agent" 事件（流式文本内容）─────────────────────────────────
	case EventAgent:
		if frame.Data == nil && frame.Payload == nil {
			return
		}
		raw := frame.Data
		if raw == nil {
			raw = frame.Payload
		}

		// 解析 agent 事件: { "sessionKey": "...", "runId": "...", "stream": "assistant"|"thinking"|"lifecycle", "data": { "text": "...", "delta": "...", "phase": "..." } }
		var agentEvent struct {
			SessionKey string `json:"sessionKey"`
			RunID      string `json:"runId"`
			Stream     string `json:"stream"`
			Data       struct {
				Text    string `json:"text"`
				Delta   string `json:"delta"`
				Phase   string `json:"phase"`
				// 嵌套的 message/partial/item 中也可能有 phase（与 Gateway resolveAssistantEventPhase 一致）
				Message *struct {
					Phase string `json:"phase"`
				} `json:"message"`
				Partial *struct {
					Phase string `json:"phase"`
				} `json:"partial"`
				Item *struct {
					Phase string `json:"phase"`
				} `json:"item"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &agentEvent); err != nil {
			if c.debug {
				fmt.Printf("  [DEBUG] agent 事件解析失败: %v\n", err)
			}
			return
		}

		// sessionKey 过滤：
		// 1. 事件 sessionKey 为空 → 内部 run（记忆插件等），直接丢弃
		// 2. 事件 sessionKey 与当前会话不匹配 → 不属于本会话，丢弃
		if agentEvent.SessionKey == "" {
			if c.debug {
				fmt.Printf("  [DEBUG] agent 事件 sessionKey 为空，丢弃（可能是记忆插件 runId=%s）\n", agentEvent.RunID)
			}
			return
		}
		if c.sessionKey != "" && agentEvent.SessionKey != c.sessionKey {
			if c.debug {
				fmt.Printf("  [DEBUG] agent 事件 sessionKey 不匹配，忽略: event=%s, local=%s\n", agentEvent.SessionKey, c.sessionKey)
			}
			return
		}

		switch agentEvent.Stream {
		case "assistant":
			// 检测 "commentary" phase — 与 Gateway 的 resolveAssistantEventPhase 一致
			// phase 可能在 data.phase / data.message.phase / data.partial.phase / data.item.phase
			isCommentary := agentEvent.Data.Phase == "commentary"
			if !isCommentary && agentEvent.Data.Message != nil && agentEvent.Data.Message.Phase == "commentary" {
				isCommentary = true
			}
			if !isCommentary && agentEvent.Data.Partial != nil && agentEvent.Data.Partial.Phase == "commentary" {
				isCommentary = true
			}
			if !isCommentary && agentEvent.Data.Item != nil && agentEvent.Data.Item.Phase == "commentary" {
				isCommentary = true
			}
			if isCommentary {
				// 这是记忆插件/内部推理输出，不展示在聊天窗口中
				if c.debug {
					preview := agentEvent.Data.Delta
					if preview == "" {
						preview = agentEvent.Data.Text
					}
					if len(preview) > 100 {
						preview = preview[:100] + "..."
					}
					fmt.Printf("  [DEBUG] assistant commentary (已过滤): %s\n", preview)
				}
				return
			}
			// 助手文本流式增量（正常回复）
			if agentEvent.Data.Delta != "" && c.onStreamDelta != nil {
				c.onStreamDelta(agentEvent.Data.Delta)
			}
		case "thinking":
			// 思考过程（内部过程，不显示在聊天窗口中）
			// 包含记忆插件、工具调用等内部输出，仅调试时打印
			if c.debug && agentEvent.Data.Delta != "" {
				fmt.Printf("  [DEBUG] thinking: %s\n", agentEvent.Data.Delta)
			}
		case "lifecycle":
			// 生命周期事件
			if c.debug {
				fmt.Printf("  [DEBUG] agent lifecycle: phase=%s\n", agentEvent.Data.Phase)
			}
			if agentEvent.Data.Phase == "endedAt" {
				// agent 运行结束
				if c.onStreamEnd != nil {
					c.onStreamEnd()
				}
			}
		default:
			// 其他 stream（如 tool, plan, command_output, compaction, patch 等）
			// 都是内部数据流，不展示在聊天窗口中，仅调试输出
			if c.debug {
				preview := agentEvent.Data.Delta
				if preview == "" {
					preview = agentEvent.Data.Text
				}
				if len(preview) > 100 {
					preview = preview[:100] + "..."
				}
				fmt.Printf("  [DEBUG] agent stream=%q (已忽略): %s\n", agentEvent.Stream, preview)
			}
		}

	// ─── OpenClaw "chat" 事件（状态汇总）─────────────────────────────────────
	case EventChat:
		if frame.Data == nil && frame.Payload == nil {
			return
		}
		raw := frame.Data
		if raw == nil {
			raw = frame.Payload
		}

		// 解析 chat 事件: { "state": "delta"|"final", "hasText": true, ... }
		var chatEvent struct {
			State            string `json:"state"`
			SessionKey       string `json:"sessionKey"`
			RunID            string `json:"runId"`
			HasText          bool   `json:"hasText"`
			HasTextDelta     bool   `json:"hasTextDelta"`
			HasThinking      bool   `json:"hasThinking"`
			HasThinkingDelta bool   `json:"hasThinkingDelta"`
			// final 状态可能包含完整文本
			Payloads []struct {
				Stream string `json:"stream"` // "assistant"|"thinking" 等
				Text   string `json:"text"`
			} `json:"payloads"`
		}
		if err := json.Unmarshal(raw, &chatEvent); err != nil {
			if c.debug {
				fmt.Printf("  [DEBUG] chat 事件解析失败: %v\n", err)
			}
			return
		}

		// sessionKey 过滤：只处理属于当前会话的 chat 事件
		if c.sessionKey != "" && chatEvent.SessionKey != "" && chatEvent.SessionKey != c.sessionKey {
			if c.debug {
				fmt.Printf("  [DEBUG] chat 事件 sessionKey 不匹配，忽略: event=%s, local=%s\n", chatEvent.SessionKey, c.sessionKey)
			}
			return
		}

		switch chatEvent.State {
		case "final":
			// 聊天最终状态 — 如果有 payloads 中的完整文本
			// 只处理 assistant stream 的 payload，跳过 thinking（记忆/场景等内部数据）
			if len(chatEvent.Payloads) > 0 {
				for _, p := range chatEvent.Payloads {
					// 只处理明确标记为 "assistant" stream 的 payload
					// 跳过所有其他 stream（thinking、空字符串等 — 包含记忆插件、场景数据、内部推理等）
					if p.Stream != "assistant" {
						if c.debug {
							// 截取前 100 字符用于调试
							preview := p.Text
							if len(preview) > 100 {
								preview = preview[:100] + "..."
							}
							fmt.Printf("  [DEBUG] chat final: 跳过非 assistant payload (stream=%q): %s\n", p.Stream, preview)
						}
						continue
					}
					if p.Text != "" && c.onStreamDelta != nil {
						// 如果之前没收到流式 delta，用 payloads 中的完整文本
						c.onStreamDelta(p.Text)
					}
				}
			}
			if c.onStreamEnd != nil {
				c.onStreamEnd()
			}
		case "delta":
			// 增量状态更新（通常不包含实际文本，只是状态标记）
			if c.debug {
				fmt.Printf("  [DEBUG] chat delta: hasText=%v, hasTextDelta=%v\n", chatEvent.HasText, chatEvent.HasTextDelta)
			}
		default:
			if c.debug {
				fmt.Printf("  [DEBUG] chat 未知 state: %s\n", chatEvent.State)
			}
		}

	// ─── 健康状态事件（静默处理）──────────────────────────────────────────────
	case EventHealth:
		// 忽略 health 事件（连接后周期广播）
		if c.debug {
			fmt.Printf("  [DEBUG] health 事件 (已忽略)\n")
		}

	// ─── Tick 心跳事件（静默处理）────────────────────────────────────────────
	case EventTick:
		// 忽略 tick 事件（服务器周期性心跳时间戳）
		if c.debug {
			fmt.Printf("  [DEBUG] tick 事件 (已忽略)\n")
		}

	// ─── 兼容旧格式：stream.delta / stream.end ───────────────────────────────
	case EventStreamDelta:
		if c.onStreamDelta != nil && frame.Data != nil {
			var deltaData struct {
				Delta   string `json:"delta"`
				Content string `json:"content"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal(frame.Data, &deltaData); err == nil {
				delta := deltaData.Delta
				if delta == "" {
					delta = deltaData.Content
				}
				if delta == "" {
					delta = deltaData.Text
				}
				if delta != "" {
					c.onStreamDelta(delta)
				}
			} else {
				var s string
				if json.Unmarshal(frame.Data, &s) == nil {
					c.onStreamDelta(s)
				}
			}
		}

	case EventStreamEnd:
		if c.onStreamEnd != nil {
			c.onStreamEnd()
		}

	case EventToolApproval:
		if c.onToolApproval != nil && frame.Data != nil {
			var approval struct {
				ApprovalID       string `json:"approvalId"`
				ToolName         string `json:"toolName"`
				ArgumentsPreview string `json:"argumentsPreview"`
			}
			if err := json.Unmarshal(frame.Data, &approval); err == nil {
				c.onToolApproval(approval.ApprovalID, approval.ToolName, approval.ArgumentsPreview)
			}
		}

	case EventAssistantMessage:
		if c.onMessage != nil {
			c.onMessage(frame)
		}

	// ─── Session 相关事件（静默处理，包含记忆/场景等内部数据）──────────────────
	case "session.message", "session.tool", "sessions.changed":
		// 这些事件包含记忆插件输出（scene_name, memories 等内部数据）
		// 不展示在聊天窗口中
		if c.debug {
			fmt.Printf("  [DEBUG] %s 事件 (已忽略)\n", frame.Event)
		}

	// ─── Presence / Heartbeat（静默处理）─────────────────────────────────────
	case "presence", "heartbeat", "cron", "shutdown":
		if c.debug {
			fmt.Printf("  [DEBUG] %s 事件 (已忽略)\n", frame.Event)
		}

	default:
		if c.debug {
			fmt.Printf("  [DEBUG] 未知事件: %s\n", frame.Event)
		}
		if c.onMessage != nil {
			c.onMessage(frame)
		}
	}
}

// closeCodeToString 将关闭码转换为可读字符串
func closeCodeToString(code int) string {
	switch code {
	case websocket.CloseNormalClosure:
		return "正常关闭"
	case websocket.CloseGoingAway:
		return "服务器关闭"
	case websocket.CloseProtocolError:
		return "协议错误"
	case websocket.CloseUnsupportedData:
		return "不支持的数据"
	case websocket.CloseNoStatusReceived:
		return "无状态码"
	case websocket.CloseAbnormalClosure:
		return "异常关闭"
	case websocket.CloseInvalidFramePayloadData:
		return "无效数据"
	case websocket.ClosePolicyViolation:
		return "策略违规"
	case websocket.CloseMessageTooBig:
		return "消息过大"
	case websocket.CloseMandatoryExtension:
		return "缺少扩展"
	case websocket.CloseInternalServerErr:
		return "服务器内部错误"
	case websocket.CloseServiceRestart:
		return "服务重启"
	case websocket.CloseTryAgainLater:
		return "请稍后重试"
	default:
		return "未知"
	}
}
