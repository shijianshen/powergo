package tool

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shijianshen/powergo/internal/rpc"
	"github.com/shijianshen/powergo/internal/task"
	"github.com/shijianshen/powergo/internal/winenc"
)

const createNoWindow = 0x08000000

// RegisterExec 注册命令执行能力。
func RegisterExec(r *Registry) {
	r.Register(&Tool{
		Name: "exec.run",
		Description: "执行命令并等待结束，返回退出码与输出。带 ?stream=1 时输出会实时推送。" +
			"适合秒级命令；长任务请用 exec.spawn。",
		Schema: BuildSchema([]Param{
			{Name: "command", Type: "string", Desc: "要执行的命令，如 dir 或 python a.py", Required: true},
			{Name: "shell", Type: "string", Desc: "解释器：cmd 或 powershell", Enum: []string{"cmd", "powershell"}, Default: "cmd"},
			{Name: "workdir", Type: "string", Desc: "工作目录，默认配置里的 work_dir"},
			{Name: "timeout", Type: "integer", Desc: "超时秒数，0 表示用默认值", Default: 0},
			{Name: "env", Type: "object", Desc: "附加环境变量，如 {\"FOO\":\"bar\"}"},
		}),
		Handler: execRun,
	})

	r.Register(&Tool{
		Name: "exec.spawn",
		Description: "后台启动命令，立即返回任务 ID。用 task.log 拉输出、task.kill 终止。" +
			"适合分钟级以上的脚本或服务。",
		Schema: BuildSchema([]Param{
			{Name: "command", Type: "string", Desc: "要执行的命令", Required: true},
			{Name: "shell", Type: "string", Desc: "解释器：cmd 或 powershell", Enum: []string{"cmd", "powershell"}, Default: "cmd"},
			{Name: "workdir", Type: "string", Desc: "工作目录"},
			{Name: "label", Type: "string", Desc: "任务标签，便于识别"},
			{Name: "env", Type: "object", Desc: "附加环境变量"},
		}),
		Handler: execSpawn,
	})
}

// RegisterTask 注册后台任务查询能力。
func RegisterTask(r *Registry) {
	r.Register(&Tool{
		Name:        "task.list",
		Description: "列出本机已启动的后台任务及状态。",
		Schema:      BuildSchema(nil),
		Handler: func(c *Context, _ json.RawMessage) (interface{}, *rpc.Error) {
			return map[string]interface{}{"tasks": c.Tasks.List()}, nil
		},
	})

	r.Register(&Tool{
		Name:        "task.log",
		Description: "增量读取后台任务输出。首次 offset 传 0，之后传上次返回的 next_offset。",
		Schema: BuildSchema([]Param{
			{Name: "id", Type: "string", Desc: "任务 ID", Required: true},
			{Name: "offset", Type: "integer", Desc: "从哪个字节位置继续读", Default: 0},
		}),
		Handler: taskLog,
	})

	r.Register(&Tool{
		Name:        "task.kill",
		Description: "终止后台任务及其全部子进程。",
		Schema: BuildSchema([]Param{
			{Name: "id", Type: "string", Desc: "任务 ID", Required: true},
		}),
		Handler: taskKill,
	})
}

type execResult struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
	Streamed   bool   `json:"streamed"`
}

func execRun(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Command string            `json:"command"`
		Shell   string            `json:"shell"`
		WorkDir string            `json:"workdir"`
		Timeout int               `json:"timeout"`
		Env     map[string]string `json:"env"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.Command) == "" {
		return nil, rpc.ErrInvalidParams("command 不能为空")
	}

	cmd := buildCommand(p.Shell, p.Command)
	if e := prepare(c, cmd, p.WorkDir, p.Env); e != nil {
		return nil, e
	}

	timeout := c.timeoutFor(p.Timeout)
	start := time.Now()

	var mu sync.Mutex
	var stdout, stderr strings.Builder

	code, timedOut, err := runStreaming(cmd, timeout, func(kind, line string) {
		mu.Lock()
		if kind == "stdout" {
			stdout.WriteString(line)
			stdout.WriteString("\n")
		} else {
			stderr.WriteString(line)
			stderr.WriteString("\n")
		}
		mu.Unlock()

		if c.Stream.Enabled() {
			c.Stream.Emit(kind, line+"\n")
		}
	})

	if err != nil && !timedOut {
		return nil, rpc.ErrInternal(err)
	}

	res := execResult{
		Command:    p.Command,
		ExitCode:   code,
		TimedOut:   timedOut,
		DurationMS: time.Since(start).Milliseconds(),
		Streamed:   c.Stream.Enabled(),
	}
	if !c.Stream.Enabled() {
		res.Stdout, _ = clip(stdout.String(), c.Cfg.MaxOutputBytes)
		res.Stderr, _ = clip(stderr.String(), c.Cfg.MaxOutputBytes)
	}
	return res, nil
}

func execSpawn(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Command string            `json:"command"`
		Shell   string            `json:"shell"`
		WorkDir string            `json:"workdir"`
		Label   string            `json:"label"`
		Env     map[string]string `json:"env"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.Command) == "" {
		return nil, rpc.ErrInvalidParams("command 不能为空")
	}

	cmd := buildCommand(p.Shell, p.Command)
	if e := prepare(c, cmd, p.WorkDir, p.Env); e != nil {
		return nil, e
	}

	label := p.Label
	if label == "" {
		label = p.Command
	}
	t, err := c.Tasks.Start(cmd, label)
	if err != nil {
		return nil, rpc.ErrInternal(err)
	}
	return map[string]interface{}{
		"id":      t.ID,
		"pid":     t.PID,
		"status":  task.StatusRunning,
		"started": t.Started,
	}, nil
}

func taskLog(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	t, ok := c.Tasks.Get(p.ID)
	if !ok {
		return nil, rpc.NewError(rpc.CodeNotFound, "任务不存在: %s", p.ID)
	}
	data, next, truncated := t.Read(p.Offset)
	return map[string]interface{}{
		"id":          p.ID,
		"output":      data,
		"next_offset": next,
		"truncated":   truncated,
		"status":      t.Status,
	}, nil
}

func taskKill(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		ID string `json:"id"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	t, ok := c.Tasks.Get(p.ID)
	if !ok {
		return nil, rpc.NewError(rpc.CodeNotFound, "任务不存在: %s", p.ID)
	}
	if err := t.Kill(); err != nil {
		return nil, rpc.NewError(rpc.CodeConflict, "%v", err)
	}
	return map[string]interface{}{"id": p.ID, "killed": true}, nil
}

// buildCommand 按 shell 类型构造命令行。
func buildCommand(shell, command string) *exec.Cmd {
	switch strings.ToLower(shell) {
	case "powershell", "ps":
		// -NoProfile 避免加载用户脚本拖慢启动，-NonInteractive 防止卡在交互提示
		return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	default:
		return exec.Command("cmd", "/c", command)
	}
}

// prepare 统一设置工作目录、环境变量与隐藏窗口标志。
func prepare(c *Context, cmd *exec.Cmd, workdir string, extraEnv map[string]string) *rpc.Error {
	if workdir != "" {
		abs, e := resolvePath(c, workdir)
		if e != nil {
			return e
		}
		cmd.Dir = abs
	} else {
		cmd.Dir = c.Cfg.WorkDir
	}

	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	// 若 PGCact 以后以 Windows 服务方式运行，子进程不加这个标志会弹黑窗
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return nil
}

// timeoutFor 把请求里的超时收敛到配置允许的区间内。
func (c *Context) timeoutFor(sec int) time.Duration {
	if sec <= 0 {
		sec = c.Cfg.DefaultTimeoutSec
	}
	if sec > c.Cfg.MaxTimeoutSec {
		sec = c.Cfg.MaxTimeoutSec
	}
	return time.Duration(sec) * time.Second
}

// runStreaming 启动进程，逐行转码输出，并处理超时强杀。
func runStreaming(cmd *exec.Cmd, timeout time.Duration, onLine func(kind, line string)) (exitCode int, timedOut bool, err error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, false, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, false, err
	}

	if err := cmd.Start(); err != nil {
		return -1, false, err
	}
	pid := cmd.Process.Pid

	var wg sync.WaitGroup
	wg.Add(2)

	pump := func(r io.Reader, kind string) {
		defer wg.Done()
		dec := &winenc.LineDecoder{}
		buf := make([]byte, 32<<10)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				for _, line := range dec.Feed(buf[:n]) {
					onLine(kind, line)
				}
			}
			if rerr != nil {
				break
			}
		}
		for _, line := range dec.Flush() {
			onLine(kind, line)
		}
	}

	go pump(stdout, "stdout")
	go pump(stderr, "stderr")

	var timedOutFlag atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOutFlag.Store(true)
		killTree(pid)
	})

	wg.Wait()
	werr := cmd.Wait()
	timer.Stop()

	if timedOutFlag.Load() {
		return -1, true, nil
	}
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			return ee.ExitCode(), false, nil
		}
		return -1, false, werr
	}
	return 0, false, nil
}

// killTree 结束整个进程树。cmd.Process.Kill 只杀直接子进程，
// bat / 脚本派生出的进程会遗留成孤儿进程继续占用文件句柄。
func killTree(pid int) {
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := k.Run(); err != nil {
		if p, perr := os.FindProcess(pid); perr == nil {
			_ = p.Kill()
		}
	}
}
