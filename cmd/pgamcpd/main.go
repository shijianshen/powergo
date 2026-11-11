// Command mcpd 是 MCP 适配层，把 PGCact 的能力以 MCP 协议暴露给 AI 客户端。
//
// 数据流：
//
//	MCP 客户端 (WorkBuddy / Claude Desktop / Cursor)
//	    │  stdio: 每行一个 JSON-RPC 消息
//	    ▼
//	  mcpd
//	    │  HTTP POST /rpc
//	    ▼
//	  PGCact (被控端)
//
// 关键约束：stdout 是协议通道，任何日志、调试输出都必须走 stderr，
// 否则会污染帧流让客户端解析失败。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/shijianshen/powergo/internal/remote"
	"github.com/shijianshen/powergo/internal/rpc"
	"github.com/shijianshen/powergo/internal/server"
)

const (
	// 支持的 MCP 协议版本，按新到旧排列
	proto20250618 = "2025-06-18"
	proto20250326 = "2025-03-26"
	proto20241105 = "2024-11-05"
	protoDefault  = proto20241105
)

type serverState struct {
	mu     sync.Mutex
	client *remote.Client
	tools  []remote.ToolInfo

	// MCP 要求工具名匹配 ^[a-zA-Z0-9_-]{1,64}$，PGCact 用的 fs.read 含点号，
	// 需要双向映射：mcpName 供 tools/list，agentMethod 供 tools/call 还原。
	mcpName     map[string]string
	agentMethod map[string]string
	host        string
}

func main() {
	log.SetOutput(io.Discard)

	var (
		flagAddr     = flag.String("addr", "", "直接指定 PGCact 地址，跳过凭据查找")
		flagToken    = flag.String("token", "", "直接指定 Token")
		flagDiscPort = flag.Int("disc-port", 7778, "UDP 发现端口")
		flagDebug    = flag.Bool("debug", false, "把调试日志写到 stderr")
	)
	flag.Parse()

	if *flagDebug {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	}

	state := &serverState{mcpName: map[string]string{}, agentMethod: map[string]string{}}
	if err := state.connect(*flagAddr, *flagToken, *flagDiscPort); err != nil {
		// 不退出：让客户端能连上并看到 tools/list 的报错信息，
		// 直接退出反而会让客户端只显示"服务器启动失败"，无从排查。
		log.Printf("初始连接失败: %v", err)
		fmt.Fprintf(os.Stderr, "[PowerGo] 初始连接失败: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[PowerGo] 已连接 %s，注册 %d 个工具\n", state.host, len(state.tools))
	}

	serve(state, *flagAddr, *flagToken, *flagDiscPort)
}

// serve 是 stdio 主循环，逐行读取 JSON-RPC 请求。
func serve(s *serverState, addr, token string, discPort int) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false) // 避免中文与 <>& 被转义，方便 AI 直接读

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var req rpc.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = out.Encode(errResponse(nil, rpc.CodeParseError, "请求不是合法 JSON: "+err.Error()))
			continue
		}

		// 通知没有 id，不需要响应
		if req.IsNotify() {
			log.Printf("收到通知: %s", req.Method)
			continue
		}

		resp := handle(s, &req, addr, token, discPort)
		if err := out.Encode(resp); err != nil {
			log.Printf("写响应失败: %v", err)
			return
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[PowerGo] 读取 stdin 失败: %v\n", err)
	}
}

func handle(s *serverState, req *rpc.Request, addr, token string, discPort int) *rpc.Response {
	switch req.Method {
	case "initialize":
		return handleInitialize(req)
	case "ping":
		return okResponse(req.ID, map[string]interface{}{})
	case "tools/list":
		return handleToolsList(s, req, addr, token, discPort)
	case "tools/call":
		return handleToolsCall(s, req, addr, token, discPort)
	default:
		// resources/prompts 等未实现的能力，明确返回不支持，客户端会自动降级
		return errResponse(req.ID, rpc.CodeMethodNotFound, "PGAmcpd 不支持该方法: "+req.Method)
	}
}

func handleInitialize(req *rpc.Request) *rpc.Response {
	// 回显客户端请求的协议版本，兼容性最好；未知版本退回我们实现的默认值
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	version := protoDefault
	switch p.ProtocolVersion {
	case proto20250618, proto20250326, proto20241105:
		version = p.ProtocolVersion
	}

	return okResponse(req.ID, map[string]interface{}{
		"protocolVersion": version,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{"listChanged": false},
		},
		"serverInfo": map[string]interface{}{
			"name":    "powergo",
			"version": server.Version,
		},
	})
}

func handleToolsList(s *serverState, req *rpc.Request, addr, token string, discPort int) *rpc.Response {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 首次调用或之前连接失败时，再尝试一次
	if len(s.tools) == 0 {
		if err := s.connect(addr, token, discPort); err != nil {
			return errResponse(req.ID, rpc.CodeInternalError,
				"无法连接被控端 上的 PGCact: "+err.Error()+
					"；请先在控制端 上执行 PGAcli pair 完成配对，并确认 PGCact 正在运行")
		}
	}

	type mcpTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}

	tools := make([]mcpTool, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, mcpTool{
			Name:        s.mcpName[t.Name],
			Description: t.Description,
			InputSchema: t.Schema,
		})
	}
	return okResponse(req.ID, map[string]interface{}{"tools": tools})
}

func handleToolsCall(s *serverState, req *rpc.Request, addr, token string, discPort int) *rpc.Response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResponse(req.ID, rpc.CodeInvalidParams, "参数格式错误: "+err.Error())
	}
	if p.Name == "" {
		return errResponse(req.ID, rpc.CodeInvalidParams, "缺少工具名")
	}

	s.mu.Lock()
	method, ok := s.agentMethod[p.Name]
	client := s.client
	s.mu.Unlock()

	if !ok {
		return errResponse(req.ID, rpc.CodeInvalidParams, "未知工具: "+p.Name)
	}
	if client == nil {
		if err := s.connect(addr, token, discPort); err != nil {
			return toolError(req.ID, "无法连接 PGCact: "+err.Error())
		}
		s.mu.Lock()
		client = s.client
		s.mu.Unlock()
	}

	result, rerr := client.Call(context.Background(), method, p.Arguments)
	if rerr != nil {
		// 工具执行失败属于"工具结果错误"，用 isError 返回，
		// 这样 AI 能看到错误内容并自行修正，而不是整个调用链中断。
		return toolError(req.ID, formatRPCError(rerr))
	}

	return toolText(req.ID, renderResult(result))
}

// ---------- 响应构造 ----------

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func okResponse(id json.RawMessage, result interface{}) *rpc.Response {
	return &rpc.Response{JSONRPC: rpc.Version, ID: id, Result: result}
}

func errResponse(id json.RawMessage, code int, msg string) *rpc.Response {
	return &rpc.Response{JSONRPC: rpc.Version, ID: id, Error: rpc.NewError(code, "%s", msg)}
}

func toolText(id json.RawMessage, text string) *rpc.Response {
	return okResponse(id, map[string]interface{}{
		"content": []contentItem{{Type: "text", Text: text}},
		"isError": false,
	})
}

func toolError(id json.RawMessage, text string) *rpc.Response {
	return okResponse(id, map[string]interface{}{
		"content": []contentItem{{Type: "text", Text: text}},
		"isError": true,
	})
}
