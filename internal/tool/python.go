package tool

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"powergo/internal/rpc"
)

// RegisterPython 注册 Python 执行能力。
func RegisterPython(r *Registry) {
	r.Register(&Tool{
		Name:        "python.detect",
		Description: "探测本机可用的 Python 解释器及其版本。",
		Schema:      BuildSchema(nil),
		Handler:     pythonDetect,
	})

	r.Register(&Tool{
		Name: "python.exec",
		Description: "执行一段 Python 代码，每次都是全新进程（不保留变量状态）。" +
			"需要连续交互或保留变量请用 python.repl。",
		Schema: BuildSchema([]Param{
			{Name: "code", Type: "string", Desc: "Python 源码", Required: true},
			{Name: "workdir", Type: "string", Desc: "工作目录，影响相对路径与 import 搜索"},
			{Name: "timeout", Type: "integer", Desc: "超时秒数", Default: 0},
			{Name: "args", Type: "array", Desc: "传给脚本的命令行参数"},
		}),
		Handler: pythonExec,
	})

	r.Register(&Tool{
		Name: "python.repl",
		Description: "在常驻 Python 会话里执行代码，变量与 import 状态跨调用保留。" +
			"适合分步做数据分析。传 reset=true 可重建会话。",
		Schema: BuildSchema([]Param{
			{Name: "code", Type: "string", Desc: "Python 源码"},
			{Name: "session", Type: "string", Desc: "会话名，默认 default，可并行多个会话", Default: "default"},
			{Name: "reset", Type: "boolean", Desc: "先销毁会话再执行", Default: false},
			{Name: "timeout", Type: "integer", Desc: "超时秒数", Default: 60},
		}),
		Handler: pythonRepl,
	})
}

type pyInfo struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// findPython 依次尝试配置项与常见命令，返回第一个可用的解释器。
func findPython(c *Context) pyInfo {
	candidates := []string{}
	if c.Cfg.PythonPath != "" {
		candidates = append(candidates, c.Cfg.PythonPath)
	}
	candidates = append(candidates, "python", "python3", "py")

	for _, cand := range candidates {
		args := []string{"--version"}
		if strings.EqualFold(cand, "py") {
			args = []string{"-3", "--version"}
		}
		out, err := exec.Command(cand, args...).CombinedOutput()
		if err != nil {
			continue
		}
		ver := strings.TrimSpace(string(out))
		if ver == "" {
			continue
		}
		return pyInfo{Path: cand, Version: ver, OK: true}
	}
	return pyInfo{OK: false, Error: "未找到可用的 Python 解释器，可在配置里指定 python_path"}
}

func pythonDetect(c *Context, _ json.RawMessage) (interface{}, *rpc.Error) {
	info := findPython(c)
	return info, nil
}

func pythonExec(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Code    string   `json:"code"`
		WorkDir string   `json:"workdir"`
		Timeout int      `json:"timeout"`
		Args    []string `json:"args"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.Code) == "" {
		return nil, rpc.ErrInvalidParams("code 不能为空")
	}

	info := findPython(c)
	if !info.OK {
		return nil, rpc.NewError(rpc.CodeNotFound, "%s", info.Error)
	}

	// 走临时文件而不是 python -c：代码里有引号、换行、中文时 -c 几乎必然出错
	dir, err := os.MkdirTemp("", "powergo_py_")
	if err != nil {
		return nil, rpc.ErrInternal(err)
	}
	defer os.RemoveAll(dir)

	script := filepath.Join(dir, "main.py")
	if err := os.WriteFile(script, []byte(p.Code), 0o600); err != nil {
		return nil, rpc.ErrInternal(err)
	}

	args := []string{"-u", script}
	args = append(args, p.Args...)

	cmd := exec.Command(info.Path, args...)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	if p.WorkDir != "" {
		abs, e := resolvePath(c, p.WorkDir)
		if e != nil {
			return nil, e
		}
		cmd.Dir = abs
	} else {
		cmd.Dir = c.Cfg.WorkDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}

	var stdout, stderr strings.Builder
	start := time.Now()
	code, timedOut, rerr := runStreaming(cmd, c.timeoutFor(p.Timeout), func(kind, line string) {
		if kind == "stdout" {
			stdout.WriteString(line + "\n")
		} else {
			stderr.WriteString(line + "\n")
		}
		if c.Stream.Enabled() {
			c.Stream.Emit(kind, line+"\n")
		}
	})
	if rerr != nil && !timedOut {
		return nil, rpc.ErrInternal(rerr)
	}

	res := map[string]interface{}{
		"exit_code":   code,
		"timed_out":   timedOut,
		"duration_ms": time.Since(start).Milliseconds(),
		"python":      info.Version,
	}
	if !c.Stream.Enabled() {
		res["stdout"], _ = clip(stdout.String(), c.Cfg.MaxOutputBytes)
		res["stderr"], _ = clip(stderr.String(), c.Cfg.MaxOutputBytes)
	}
	return res, nil
}

func pythonRepl(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Code    string `json:"code"`
		Session string `json:"session"`
		Reset   bool   `json:"reset"`
		Timeout int    `json:"timeout"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.Code) == "" {
		return nil, rpc.ErrInvalidParams("code 不能为空")
	}
	if p.Session == "" {
		p.Session = "default"
	}

	sess, e := c.Py.Get(p.Session, c)
	if e != nil {
		return nil, e
	}
	if p.Reset {
		c.Py.Close(p.Session)
		sess, e = c.Py.Get(p.Session, c)
		if e != nil {
			return nil, e
		}
	}

	out, runErr, timedOut := sess.Eval(p.Code, c.timeoutFor(p.Timeout))
	if timedOut {
		// 会话卡死或进入死循环，直接销毁，下次调用自动重建
		c.Py.Close(p.Session)
		return nil, rpc.NewError(rpc.CodeTimeout, "Python 会话执行超时，已重置该会话")
	}
	return map[string]interface{}{
		"session": p.Session,
		"output":  out,
		"ok":      runErr == "",
		"error":   runErr,
	}, nil
}
