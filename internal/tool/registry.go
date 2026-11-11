// Package tool 定义可远程调用的能力，并维护方法名到实现的注册表。
//
// 每个 Tool 同时携带 JSON Schema，MCP 适配层可以直接把它导出成 tools/list，
// 不需要为两种协议各维护一套定义。
package tool

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"powergo/internal/config"
	"powergo/internal/rpc"
	"powergo/internal/sandbox"
	"powergo/internal/task"
)

// Context 是一次调用的执行环境。
type Context struct {
	Ctx    context.Context
	Stream *rpc.Stream
	Cfg    *config.Config
	Box    *sandbox.Sandbox
	Tasks  *task.Manager
	Py     *ReplManager

	// Runtime 字段由 PGCact 启动时填充，用于 sys.info 自检
	Started time.Time
	Version string
	Addr    string
}

// Handler 是工具实现。返回值和错误对象二选一。
type Handler func(c *Context, params json.RawMessage) (interface{}, *rpc.Error)

// Tool 是可调用能力。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema"`
	Handler     Handler         `json:"-"`
}

// Registry 是方法注册表，并发安全。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*Tool)}
}

// Register 注册工具，同名覆盖。
func (r *Registry) Register(t *Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = make(map[string]*Tool)
	}
	r.tools[t.Name] = t
}

// Get 查找工具。
func (r *Registry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 按名称排序返回全部工具，保证 tools/list 输出稳定。
func (r *Registry) List() []*Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Call 分发一次调用，捕获 handler 的 panic 以免打挂服务。
func (r *Registry) Call(c *Context, method string, params json.RawMessage) (result interface{}, rerr *rpc.Error) {
	t, ok := r.Get(method)
	if !ok {
		return nil, rpc.ErrMethodNotFound(method)
	}
	defer func() {
		if p := recover(); p != nil {
			result = nil
			rerr = rpc.NewError(rpc.CodeInternalError, "工具 %s 执行异常: %v", method, p)
		}
	}()
	return t.Handler(c, params)
}

// Bind 把 params 解析进目标结构体，空参数视为 {}。
func Bind(params json.RawMessage, v interface{}) *rpc.Error {
	if len(params) == 0 || string(params) == "null" {
		params = []byte("{}")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return rpc.ErrInvalidParams("参数解析失败: %v", err)
	}
	return nil
}

// Param 描述一个入参，用于生成 JSON Schema。
type Param struct {
	Name     string
	Type     string
	Desc     string
	Enum     []string
	Required bool
	Default  interface{}
}

// BuildSchema 生成 JSON Schema 对象。手写 schema 太啰嗦，统一由这里产出。
func BuildSchema(params []Param) json.RawMessage {
	props := make(map[string]interface{}, len(params))
	required := make([]string, 0, len(params))

	for _, p := range params {
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		item := map[string]interface{}{"type": typ}
		if p.Desc != "" {
			item["description"] = p.Desc
		}
		if len(p.Enum) > 0 {
			item["enum"] = p.Enum
		}
		if p.Default != nil {
			item["default"] = p.Default
		}
		props[p.Name] = item
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]interface{}{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, _ := json.Marshal(schema)
	return b
}
