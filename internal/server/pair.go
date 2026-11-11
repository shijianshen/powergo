package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pairRequest 是客户端提交的配对请求。
type pairRequest struct {
	Code   string `json:"code"`
	Client string `json:"client"`
}

// pairResponse 是配对结果。成功时返回长期有效的 Token。
type pairResponse struct {
	OK       bool   `json:"ok"`
	Locked   bool   `json:"locked,omitempty"`
	Error    string `json:"error,omitempty"`
	Token    string `json:"token,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	Host     string `json:"host,omitempty"`
	HTTPPort int    `json:"http_port,omitempty"`
	Version  string `json:"version,omitempty"`
}

const (
	pairMaxFails  = 5
	pairLockTime  = 60 * time.Second
	pairFailPurge = 10 * time.Minute
)

// pairGuard 限制单个 IP 的配对尝试次数，防止 6 位码被穷举。
type pairGuard struct {
	mu    sync.Mutex
	fails map[string]*pairFail
}

type pairFail struct {
	count int
	last  time.Time
}

func newPairGuard() *pairGuard {
	return &pairGuard{fails: make(map[string]*pairFail)}
}

// locked 判断该 IP 是否处于锁定状态。
func (g *pairGuard) locked(ip string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	f, ok := g.fails[ip]
	if !ok {
		return false, 0
	}
	if time.Since(f.last) > pairLockTime {
		delete(g.fails, ip)
		return false, 0
	}
	if f.count >= pairMaxFails {
		return true, pairLockTime - time.Since(f.last)
	}
	return false, 0
}

// fail 记录一次失败。
func (g *pairGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 顺带清理长期不活跃的记录，避免 map 无限增长
	if len(g.fails) > 512 {
		for k, v := range g.fails {
			if time.Since(v.last) > pairFailPurge {
				delete(g.fails, k)
			}
		}
	}

	f, ok := g.fails[ip]
	if !ok {
		g.fails[ip] = &pairFail{count: 1, last: time.Now()}
		return
	}
	f.count++
	f.last = time.Now()
}

func (g *pairGuard) reset(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

// handlePair 处理首次配对。
//
// 这是整个服务里唯一不校验 Token 的端点 —— 因为调用它的客户端还没有 Token。
// 安全边界是：配对码只打印在电脑 C 的控制台窗口上，需要物理接触才能看到。
// 再加单 IP 5 次失败锁定 60 秒，6 位码猜中的概率被压到可忽略。
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if !s.Cfg.PairingOn() {
		writeJSON(w, pairResponse{OK: false, Error: "本机未开放配对，请在 agentd.json 里把 pairing_enabled 设为 true"})
		return
	}
	if !s.ipAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ip := clientIP(r)
	if locked, remain := s.guard.locked(ip); locked {
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, pairResponse{
			OK:     false,
			Locked: true,
			Error:  "尝试次数过多，请 " + remain.Round(time.Second).String() + " 后再试",
		})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeJSON(w, pairResponse{OK: false, Error: "读取请求失败"})
		return
	}

	var req pairRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, pairResponse{OK: false, Error: "请求格式错误"})
		return
	}

	code := strings.TrimSpace(req.Code)
	if code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(s.Cfg.PairCode)) != 1 {
		s.guard.fail(ip)
		log.Printf("配对失败：来自 %s (%s) 的配对码不正确", ip, orUnknown(req.Client))
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, pairResponse{OK: false, Error: "配对码不正确"})
		return
	}

	s.guard.reset(ip)

	// 配对码用一次就换，防止旁观者复用
	newCode, err := s.Cfg.RotatePairCode()
	if err != nil {
		log.Printf("配对码轮换失败: %v", err)
	}
	if s.CfgPath != "" {
		if err := s.Cfg.Save(s.CfgPath); err != nil {
			log.Printf("回写新配对码失败: %v", err)
		}
	}

	host, _ := hostname()
	log.Printf("配对成功：%s (%s) 已接入，新配对码 %s", orUnknown(req.Client), ip, newCode)

	writeJSON(w, pairResponse{
		OK:       true,
		Token:    s.Cfg.Token,
		NodeID:   s.Cfg.NodeID,
		Host:     host,
		HTTPPort: portFromListen(s.Cfg.Listen),
		Version:  Version,
	})
}

func orUnknown(s string) string {
	if s == "" {
		return "未知客户端"
	}
	return s
}

// portFromListen 从 "0.0.0.0:7777" 这类监听地址里取出端口号。
func portFromListen(addr string) int {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return 7777
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 7777
	}
	return p
}
