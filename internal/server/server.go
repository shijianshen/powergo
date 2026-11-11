// Package server 提供 HTTP 层的 JSON-RPC 入口、鉴权与请求日志。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"juyuagent/internal/config"
	"juyuagent/internal/rpc"
	"juyuagent/internal/sandbox"
	"juyuagent/internal/task"
	"juyuagent/internal/tool"
)

const (
	// Version 会在 /health 与 sys.info 中返回，便于确认电脑 C 上跑的是哪一版
	Version = "0.1.0"

	maxBodyBytes = 16 << 20
)

// Server 组合所有依赖，暴露统一的 http.Handler。
type Server struct {
	Cfg     *config.Config
	Box     *sandbox.Sandbox
	Tools   *tool.Registry
	Tasks   *task.Manager
	Py      *tool.ReplManager
	Started time.Time
	Addr    string

	// CfgPath 用于配对成功后回写轮换后的配对码
	CfgPath string

	// guard 限制配对尝试频率
	guard *pairGuard
}

// New 装配服务依赖。
func New(cfg *config.Config) *Server {
	box := sandbox.New(cfg.AllowedRoots)

	reg := tool.NewRegistry()
	tool.RegisterFS(reg)
	tool.RegisterExec(reg)
	tool.RegisterTask(reg)
	tool.RegisterPython(reg)
	tool.RegisterSys(reg)

	return &Server{
		Cfg:     cfg,
		Box:     box,
		Tools:   reg,
		Tasks:   task.NewManager(64),
		Py:      tool.NewReplManager(),
		Started: time.Now(),
		guard:   newPairGuard(),
	}
}

// Handler 返回带完整中间件的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/pair", s.handlePair)
	mux.HandleFunc("/rpc", s.withAuth(s.handleRPC))
	mux.HandleFunc("/tools", s.withAuth(s.handleTools))

	return s.withCORS(s.withLogging(mux))
}

// Close 回收后台资源。
func (s *Server) Close() {
	s.Py.CloseAll()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	host, _ := hostname()
	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"version": Version,
		"host":    host,
		"uptime":  int64(time.Since(s.Started).Seconds()),
		"roots":   len(s.Box.Roots()),
	})
}

// handleTools 返回工具清单，方便 agentctl 直接展示可用能力。
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	type toolBrief struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"inputSchema"`
	}
	list := s.Tools.List()
	out := make([]toolBrief, 0, len(list))
	for _, t := range list {
		out = append(out, toolBrief{t.Name, t.Description, t.Schema})
	}
	writeJSON(w, map[string]interface{}{"count": len(out), "tools": out})
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, nil, rpc.NewError(rpc.CodeInvalidRequest, "读取请求体失败: %v", err))
		return
	}

	var req rpc.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, nil, rpc.ErrParse(err))
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != rpc.Version {
		writeError(w, req.ID, rpc.NewError(rpc.CodeInvalidRequest, "不支持的 jsonrpc 版本: %s", req.JSONRPC))
		return
	}
	if req.Method == "" {
		writeError(w, req.ID, rpc.NewError(rpc.CodeInvalidRequest, "缺少 method 字段"))
		return
	}

	wantStream := r.URL.Query().Get("stream") == "1"
	stream := rpc.NewStream(w, wantStream)

	if wantStream {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flush(w)
	}

	start := time.Now()
	c := &tool.Context{
		Ctx:     r.Context(),
		Stream:  stream,
		Cfg:     s.Cfg,
		Box:     s.Box,
		Tasks:   s.Tasks,
		Py:      s.Py,
		Started: s.Started,
		Version: Version,
		Addr:    s.Addr,
	}

	result, rerr := s.Tools.Call(c, req.Method, req.Params)

	resp := rpc.Response{JSONRPC: rpc.Version, ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}

	cost := time.Since(start)
	if rerr != nil {
		log.Printf("rpc %s 失败 code=%d 耗时=%s: %s", req.Method, rerr.Code, cost, rerr.Message)
	} else {
		log.Printf("rpc %s ok 耗时=%s", req.Method, cost)
	}

	if wantStream {
		line, merr := json.Marshal(resp)
		if merr == nil {
			_, _ = w.Write(append(line, '\n'))
			flush(w)
		}
		return
	}
	writeJSON(w, resp)
}

// withAuth 校验 IP 白名单与 Bearer Token。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ipAllowed(r) {
			log.Printf("拒绝来自 %s 的连接：IP 不在白名单", clientIP(r))
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !s.tokenValid(r) {
			log.Printf("拒绝来自 %s 的请求：Token 无效", clientIP(r))
			writeError(w, nil, rpc.NewError(rpc.CodeUnauthorized, "鉴权失败，请检查 Token"))
			return
		}
		next(w, r)
	}
}

func (s *Server) ipAllowed(r *http.Request) bool {
	if len(s.Cfg.AllowIP) == 0 {
		return true
	}
	ip := clientIP(r)
	for _, allowed := range s.Cfg.AllowIP {
		if allowed == ip {
			return true
		}
		// 支持 192.168.1.0/24 这类网段
		if strings.Contains(allowed, "/") {
			if _, cidr, err := net.ParseCIDR(allowed); err == nil {
				if parsed := net.ParseIP(ip); parsed != nil && cidr.Contains(parsed) {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) tokenValid(r *http.Request) bool {
	got := strings.TrimSpace(r.Header.Get("X-Agent-Token"))
	if got == "" {
		if auth := r.Header.Get("Authorization"); auth != "" {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" || s.Cfg.Token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Cfg.Token)) == 1
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic 已恢复: %v", p)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Agent-Token")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, id json.RawMessage, e *rpc.Error) {
	writeJSON(w, rpc.Response{JSONRPC: rpc.Version, ID: id, Error: e})
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func hostname() (string, error) {
	return os.Hostname()
}
