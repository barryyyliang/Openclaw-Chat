package main

import "encoding/json"

// ═══════════════════════════════════════════════════════════════════════════════
// OpenClaw Gateway WebSocket 协议 v4
//
// 帧类型：
//   1. ConnectFrame (Client→Server) — 握手认证，必须是连接后第一条消息
//   2. RequestFrame (Client→Server) — RPC 方法调用
//   3. ResponseFrame (Server→Client) — RPC 响应
//   4. EventFrame (Server→Client) — 服务器主动推送事件
// ═══════════════════════════════════════════════════════════════════════════════

// ─── Connect 握手帧 ─────────────────────────────────────────────────────────────

// ConnectRequest 客户端收到 challenge 后发送的握手请求（外层为 req 帧）
type ConnectRequest struct {
	Type   string        `json:"type"`   // 固定为 "req"
	ID     string        `json:"id"`     // 请求 ID
	Method string        `json:"method"` // 固定为 "connect"
	Params ConnectParams `json:"params"` // 连接参数
}

// ConnectParams connect 方法的参数
type ConnectParams struct {
	MinProtocol int          `json:"minProtocol"`            // 客户端支持的最低协议版本
	MaxProtocol int          `json:"maxProtocol"`            // 客户端支持的最高协议版本
	Client      ClientInfo   `json:"client"`                 // 客户端元数据
	Role        string       `json:"role"`                   // 请求角色: "operator" | "node"
	Scopes      []string     `json:"scopes,omitempty"`       // 请求的权限范围
	Auth        *ConnectAuth `json:"auth,omitempty"`         // 认证凭据
	Device      *DeviceInfo  `json:"device,omitempty"`       // 设备信息（含回传 nonce）
	Locale      string       `json:"locale,omitempty"`       // 区域设置
	UserAgent   string       `json:"userAgent,omitempty"`    // 用户代理
}

// ClientInfo 客户端元数据
type ClientInfo struct {
	ID       string `json:"id"`                 // 客户端标识（枚举: cli, webchat-ui, gateway-client, openclaw-macos 等）
	Version  string `json:"version"`            // 客户端版本
	Platform string `json:"platform,omitempty"` // 平台: "macos" | "linux" | "windows" | "web"
	Mode     string `json:"mode,omitempty"`     // 模式（枚举: cli, ui, webchat, backend, node, probe, test）
}

// ConnectAuth 认证信息
type ConnectAuth struct {
	Token          string `json:"token,omitempty"`          // 共享密钥 token
	Password       string `json:"password,omitempty"`       // 密码认证
	DeviceToken    string `json:"deviceToken,omitempty"`    // 设备 token（已配对设备重连用）
	BootstrapToken string `json:"bootstrapToken,omitempty"` // 首次配对引导 token
}

// DeviceInfo 设备身份信息（Ed25519 签名认证）
type DeviceInfo struct {
	ID        string `json:"id"`        // 设备 ID = SHA256(publicKey).hex()
	PublicKey string `json:"publicKey"` // Ed25519 公钥（base64url 编码，32字节）
	Signature string `json:"signature"` // Ed25519 签名（base64url 编码，64字节）
	SignedAt  int64  `json:"signedAt"`  // 签名时间戳（Unix 毫秒）
	Nonce     string `json:"nonce"`     // 回传 connect.challenge 中的 nonce
}

// ChallengePayload connect.challenge 事件的 payload
type ChallengePayload struct {
	Nonce string `json:"nonce"` // 服务端生成的一次性随机数
	Ts    int64  `json:"ts"`    // 服务端时间戳（毫秒）
}

// ─── Request 帧（Client → Server RPC 调用）──────────────────────────────────────

// RequestFrame 客户端发送的 RPC 请求
type RequestFrame struct {
	Type   string      `json:"type"`             // 固定为 "req"
	ID     interface{} `json:"id"`               // 唯一请求 ID (string 或 number)
	Method string      `json:"method"`           // RPC 方法名: "chat.send", "sessions.list" 等
	Params interface{} `json:"params,omitempty"` // 方法参数
}

// ChatSendParams chat.send 方法的参数
type ChatSendParams struct {
	SessionKey     string `json:"sessionKey"`               // 会话 Key (如 "agent:main:main")
	Message        string `json:"message"`                  // 消息内容（纯字符串，role 隐含为 user）
	IdempotencyKey string `json:"idempotencyKey"`           // 幂等键（唯一，防重复发送）
	Thinking       bool   `json:"thinking,omitempty"`       // 是否启用 thinking 模式
	Deliver        bool   `json:"deliver,omitempty"`        // 是否投递/广播
	TimeoutMs      int    `json:"timeoutMs,omitempty"`      // 超时毫秒数
}

// ─── Response 帧（Server → Client）──────────────────────────────────────────────

// ResponseFrame 服务器返回的 RPC 响应
type ResponseFrame struct {
	ID     interface{}     `json:"id"`               // 与请求对应的 ID
	OK     bool            `json:"ok"`               // 是否成功
	Result json.RawMessage `json:"result,omitempty"` // 成功时的返回数据
	Error  *RPCError       `json:"error,omitempty"`  // 失败时的错误信息
}

// RPCError RPC 错误
type RPCError struct {
	Code    string `json:"code"`              // 错误码: INVALID_REQUEST, NOT_LINKED 等
	Message string `json:"message,omitempty"` // 错误描述
}

// ─── Event 帧（Server → Client 主动推送）────────────────────────────────────────

// EventFrame 服务器推送的事件
type EventFrame struct {
	Event        string          `json:"event"`                    // 事件类型
	Data         json.RawMessage `json:"data,omitempty"`           // 事件数据
	Seq          *int            `json:"seq,omitempty"`            // 序列号
	StateVersion json.RawMessage `json:"stateVersion,omitempty"`   // 状态版本（可能是 int 或 object）
}

// ─── hello-ok 响应 payload ──────────────────────────────────────────────────────

// HelloOKPayload hello-ok 响应的 payload
type HelloOKPayload struct {
	SessionID      string   `json:"sessionId,omitempty"`
	GatewayVersion string   `json:"gatewayVersion,omitempty"`
	Protocol       int      `json:"protocol,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	Policy         *Policy  `json:"policy,omitempty"`
}

// Policy 连接策略
type Policy struct {
	MaxPayload int `json:"maxPayload,omitempty"` // 最大消息大小（字节）
}

// ─── 通用服务器帧（用于初步解析确定帧类型）──────────────────────────────────────

// ServerFrame 通用的服务器帧结构（用于区分 response vs event vs hello-ok）
type ServerFrame struct {
	// Response 帧字段
	ID     interface{}     `json:"id,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`

	// Event 帧字段
	Event        string          `json:"event,omitempty"`
	Data         json.RawMessage `json:"data,omitempty"`
	Seq          *int            `json:"seq,omitempty"`
	StateVersion json.RawMessage `json:"stateVersion,omitempty"` // 可能是 int 或 object

	// 兼容旧格式（hello-ok 等）
	Type    string          `json:"type,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// IsResponse 判断是否为 Response 帧
func (f *ServerFrame) IsResponse() bool {
	return f.ID != nil && f.OK != nil
}

// IsEvent 判断是否为 Event 帧
func (f *ServerFrame) IsEvent() bool {
	return f.Event != ""
}

// IsTyped 判断是否为带 type 字段的帧（如 hello-ok, unauthorized）
func (f *ServerFrame) IsTyped() bool {
	return f.Type != ""
}

// ─── 事件类型常量 ────────────────────────────────────────────────────────────────

const (
	// 连接相关
	TypeHelloOK      = "hello-ok"
	TypeUnauthorized = "unauthorized"

	// 事件类型
	EventConnectChallenge  = "connect.challenge"
	EventStreamDelta       = "stream.delta"     // 旧版兼容
	EventStreamEnd         = "stream.end"       // 旧版兼容
	EventStreamStart       = "stream.start"     // 旧版兼容
	EventAssistantMessage  = "assistant.message" // 旧版兼容
	EventToolApproval      = "tool.approval"
	EventError             = "error"

	// OpenClaw 实际使用的事件名
	EventAgent  = "agent"  // agent 事件（流式文本：assistant/thinking/lifecycle）
	EventChat   = "chat"   // chat 事件（状态汇总：delta/final）
	EventHealth = "health" // 健康状态广播
	EventTick   = "tick"   // 心跳 tick 事件（周期性时间戳）

	// RPC 方法名
	MethodChatSend     = "chat.send"
	MethodSessionsList = "sessions.list"

	// 错误码
	ErrInvalidRequest   = "INVALID_REQUEST"
	ErrNotLinked        = "NOT_LINKED"
	ErrNotPaired        = "NOT_PAIRED"
	ErrAgentTimeout     = "AGENT_TIMEOUT"
	ErrApprovalNotFound = "APPROVAL_NOT_FOUND"
	ErrUnavailable      = "UNAVAILABLE"
)

// ─── 协议版本 ────────────────────────────────────────────────────────────────────

const (
	ProtocolVersion = 4 // 当前协议版本
)

// parseServerFrame 解析服务器发来的帧
func parseServerFrame(data []byte) (*ServerFrame, error) {
	var frame ServerFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, err
	}
	return &frame, nil
}
