package tool

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"juyuagent/internal/rpc"
)

// driverSrc 是常驻会话的解释器外壳。
//
// 用「行分隔 JSON」而不是人肉哨兵，彻底避开提示符干扰和输出截断误判：
// 每条请求是一行 JSON，Python 侧回一行 JSON，双方都只按 \n 分帧。
const driverSrc = `# -*- coding: utf-8 -*-
import sys, json, io, traceback, contextlib

_ns = {"__name__": "__main__"}
_real_out = sys.stdout
sys.stdout = sys.stderr

class _NoStdin(object):
    def readline(self, *a, **k):
        raise EOFError("stdin is not available inside agent session")
    def read(self, *a, **k):
        raise EOFError("stdin is not available inside agent session")

_NOIN = _NoStdin()

def _run(code):
    buf = io.StringIO()
    old_in = sys.stdin
    sys.stdin = _NOIN
    try:
        with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
            try:
                val = eval(compile(code, "<agent>", "eval"), _ns)
                if val is not None:
                    print(repr(val))
            except SyntaxError:
                exec(compile(code, "<agent>", "exec"), _ns)
        return True, buf.getvalue(), ""
    except BaseException:
        return False, buf.getvalue(), traceback.format_exc()
    finally:
        sys.stdin = old_in

while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        req = json.loads(line)
    except Exception:
        continue
    ok, out, err = _run(req.get("code", ""))
    _real_out.write(json.dumps({"ok": ok, "output": out, "error": err}) + "\n")
    _real_out.flush()
`

// PySession 是一个常驻的 Python 进程，变量与 import 状态跨调用保留。
type PySession struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	mu     sync.Mutex
	dead   bool
}

// ReplManager 管理多个具名会话。
type ReplManager struct {
	mu       sync.Mutex
	sessions map[string]*PySession
}

// NewReplManager 创建会话管理器。
func NewReplManager() *ReplManager {
	return &ReplManager{sessions: make(map[string]*PySession)}
}

// Get 取已存在的会话，不存在则新建。
func (m *ReplManager) Get(name string, c *Context) (*PySession, *rpc.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[name]; ok && s.alive() {
		return s, nil
	}
	s, err := launchSession(name, c)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "启动 Python 会话失败: %v", err)
	}
	m.sessions[name] = s
	return s, nil
}

// Close 销毁指定会话。
func (m *ReplManager) Close(name string) {
	m.mu.Lock()
	s, ok := m.sessions[name]
	delete(m.sessions, name)
	m.mu.Unlock()
	if ok {
		s.close()
	}
}

// CloseAll 在服务退出时回收全部子进程。
func (m *ReplManager) CloseAll() {
	m.mu.Lock()
	all := make([]*PySession, 0, len(m.sessions))
	for k, s := range m.sessions {
		all = append(all, s)
		delete(m.sessions, k)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

func launchSession(name string, c *Context) (*PySession, error) {
	info := findPython(c)
	if !info.OK {
		return nil, errString(info.Error)
	}

	dir := filepath.Join(os.TempDir(), "juyu_repl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	script := filepath.Join(dir, "driver.py")
	if err := os.WriteFile(script, []byte(driverSrc), 0o600); err != nil {
		return nil, err
	}

	cmd := exec.Command(info.Path, "-u", script)
	cmd.Dir = c.Cfg.WorkDir
	cmd.Env = append(os.Environ(),
		"PYTHONIOENCODING=utf-8",
		"PYTHONUTF8=1",
		"PYTHONUNBUFFERED=1",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// 必须排空 stderr，否则解释器报错写满管道会把整个会话卡死
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	return &PySession{
		name:   name,
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(stdout, 64<<10),
	}, nil
}

// Eval 执行一段代码并返回输出。timedOut 为 true 时会话已被判死。
func (s *PySession) Eval(code string, timeout time.Duration) (output string, errText string, timedOut bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dead {
		return "", "会话已失效，请重新执行以重建", false
	}

	req, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", err.Error(), false
	}
	req = append(req, '\n')
	if _, err := s.stdin.Write(req); err != nil {
		s.dead = true
		return "", "写入会话失败: " + err.Error(), false
	}

	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, rerr := s.reader.ReadBytes('\n')
		ch <- readResult{line, rerr}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			s.dead = true
			return "", "会话读取失败: " + r.err.Error(), false
		}
		var resp struct {
			OK     bool   `json:"ok"`
			Output string `json:"output"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(r.line, &resp); err != nil {
			return "", "会话响应解析失败: " + err.Error(), false
		}
		return resp.Output, resp.Error, false
	case <-time.After(timeout):
		// 死循环或长时间阻塞，直接杀掉，下次调用自动重建
		s.dead = true
		if s.cmd.Process != nil {
			killTree(s.cmd.Process.Pid)
		}
		return "", "", true
	}
}

func (s *PySession) alive() bool {
	return !s.dead
}

func (s *PySession) close() {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()

	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		killTree(s.cmd.Process.Pid)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
