// Package rpc 定义 JSON-RPC 2.0 消息结构与错误码。
// 与 MCP 共用同一套消息格式，后续接入 MCP 时无需重写协议层。
package rpc

import (
	"encoding/json"
	"fmt"
)

const Version = "2.0"

// 标准 JSON-RPC 2.0 错误码
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// 业务错误码（预留区间，避免与标准码冲突）
const (
	CodeUnauthorized  = 1001
	CodeSandboxDenied = 1002
	CodeNotFound      = 1003
	CodeTimeout       = 1004
	CodeConflict      = 1005
	CodeBusy          = 1006
)

// Request 是客户端请求。ID 为 nil 表示通知（notification）。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotify 判断是否为单向通知。
func (r *Request) IsNotify() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Error 是 JSON-RPC 错误对象。
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// Response 是服务端响应。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification 是服务端主动推送（流式输出用）。
type Notification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// NewError 构造错误对象。
func NewError(code int, format string, args ...interface{}) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithData 附加结构化信息，便于 Agent 纠错。
func (e *Error) WithData(data interface{}) *Error {
	e.Data = data
	return e
}

func ErrParse(err error) *Error {
	return NewError(CodeParseError, "解析请求失败: %v", err)
}

func ErrMethodNotFound(m string) *Error {
	return NewError(CodeMethodNotFound, "未知方法: %s", m)
}

func ErrInvalidParams(format string, args ...interface{}) *Error {
	return NewError(CodeInvalidParams, format, args...)
}

func ErrInternal(err error) *Error {
	return NewError(CodeInternalError, "内部错误: %v", err)
}
