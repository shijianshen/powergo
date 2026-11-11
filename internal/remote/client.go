// Package remote 封装与 PGCact 的 HTTP+JSON-RPC 通信，以及客户端凭据管理。
//
// PGAcli 与 mcpd 共用这一层，避免两套实现各自漂移。
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"powergo/internal/rpc"
)

// Client 是与 PGCact 通信的 HTTP 客户端。
type Client struct {
	addr  string
	token string
	http  *http.Client
}

// NormalizeAddr 规范化服务地址：补协议前缀、去掉尾部斜杠。
func NormalizeAddr(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ""
	}
	if !strings.HasPrefix(a, "http://") && !strings.HasPrefix(a, "https://") {
		a = "http://" + a
	}
	return strings.TrimRight(a, "/")
}

// NewClient 创建客户端；addr 允许不带 http:// 前缀。
func NewClient(addr, token string) *Client {
	return &Client{
		addr:  NormalizeAddr(addr),
		token: token,
		http: &http.Client{
			// 长任务可能跑很久，不设整体超时，靠 PGCact 侧的超时控制
			Timeout: 0,
		},
	}
}

// Health 探活，不需要 Token。
func (c *Client) Health(ctx context.Context) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Call 发起一次普通 JSON-RPC 调用。
func (c *Client) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, *rpc.Error) {
	return c.do(ctx, method, params, false, nil)
}

// CallStream 发起流式调用，输出片段通过 emit 实时回调。
func (c *Client) CallStream(ctx context.Context, method string, params interface{}, emit func(kind, chunk string)) (json.RawMessage, *rpc.Error) {
	return c.do(ctx, method, params, true, emit)
}

func (c *Client) do(ctx context.Context, method string, params interface{}, stream bool, emit func(kind, chunk string)) (json.RawMessage, *rpc.Error) {
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "构造请求失败: %v", err)
	}

	url := c.addr + "/rpc"
	if stream {
		url += "?stream=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "连接 PGCact 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, rpc.NewError(rpc.CodeSandboxDenied, "来源 IP 被拒绝，请检查 PGCact 的 allow_ip 配置")
	}

	if !stream {
		return decodeResponse(resp.Body)
	}

	// 流式：逐行读 NDJSON，先处理中间输出通知，最后一条是最终响应
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var final json.RawMessage
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpc.Error      `json:"error"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if env.Method == "task.output" {
			var p struct {
				Stream string `json:"stream"`
				Chunk  string `json:"chunk"`
			}
			_ = json.Unmarshal(env.Params, &p)
			if emit != nil {
				emit(p.Stream, p.Chunk)
			}
			continue
		}
		if len(env.ID) > 0 && string(env.ID) != "null" {
			final = append(final[:0], line...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "读取流式响应失败: %v", err)
	}
	if final == nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "服务端未返回结果")
	}

	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *rpc.Error      `json:"error"`
	}
	if err := json.Unmarshal(final, &env); err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "解析结果失败: %v", err)
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Result, nil
}

func decodeResponse(r io.Reader) (json.RawMessage, *rpc.Error) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *rpc.Error      `json:"error"`
	}
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "解析响应失败: %v", err)
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Result, nil
}

// unwrap 把结果反序列化到目标结构，并给出可读错误。
func unwrap(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 {
		return errors.New("服务端返回空结果")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("结果格式异常: %w", err)
	}
	return nil
}

// Tools 拉取服务端工具清单，需要 Token。
func (c *Client) ToolsRaw(ctx context.Context) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/tools", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Agent-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取工具清单失败，HTTP %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Addr 返回连接的基地址。
func (c *Client) Addr() string { return c.addr }

// Token 返回当前凭据，供调试与日志使用。
func (c *Client) Token() string { return c.token }

// ToolInfo 是服务端工具的描述，直接对应 MCP 的 tools/list 条目。
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema"`
}

// Tools 拉取服务端工具清单。
func (c *Client) Tools(ctx context.Context) ([]ToolInfo, error) {
	raw, rerr := c.ToolsRaw(ctx)
	if rerr != nil {
		return nil, rerr
	}
	items, ok := raw["tools"].([]interface{})
	if !ok {
		return nil, errors.New("工具清单格式异常")
	}
	out := make([]ToolInfo, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		var t ToolInfo
		if json.Unmarshal(b, &t) == nil && t.Name != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// PairReply 是配对接口的响应。
type PairReply struct {
	OK       bool   `json:"ok"`
	Locked   bool   `json:"locked,omitempty"`
	Error    string `json:"error,omitempty"`
	Token    string `json:"token,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	Host     string `json:"host,omitempty"`
	HTTPPort int    `json:"http_port,omitempty"`
	Version  string `json:"version,omitempty"`
}

// Pair 用配对码换取长期 Token。
func (c *Client) Pair(ctx context.Context, code, clientName string) (*PairReply, error) {
	body, err := json.Marshal(map[string]string{"code": code, "client": clientName})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+"/pair", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return nil, err
	}
	var reply PairReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("服务端返回了非预期内容（HTTP %d）", resp.StatusCode)
	}
	return &reply, nil
}

// Unwrap 把结果反序列化到目标结构。
func Unwrap(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 {
		return errors.New("服务端返回空结果")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("结果格式异常: %w", err)
	}
	return nil
}
