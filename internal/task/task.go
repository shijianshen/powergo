// Package task 管理后台长任务：启动、增量取日志、终止。
//
// Agent 经常需要跑分钟级的命令或脚本，HTTP 请求等不了那么久，
// 所以异步任务统一走 spawn + 轮询 task.log 的模式。
package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"juyuagent/internal/winenc"
)

// 任务状态
const (
	StatusRunning = "running"
	StatusExited  = "exited"
	StatusKilled  = "killed"
	StatusFailed  = "failed"
)

// Task 是一个后台运行的子进程。
type Task struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Status   string    `json:"status"`
	ExitCode int       `json:"exit_code"`
	Started  time.Time `json:"started_at"`
	Ended    time.Time `json:"ended_at,omitempty"`
	PID      int       `json:"pid"`

	mu   sync.Mutex
	buf  *RingBuffer
	cmd  *exec.Cmd
	done chan struct{}
	err  string
}

// Snapshot 是可安全序列化的任务状态快照。
type Snapshot struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Status   string    `json:"status"`
	ExitCode int       `json:"exit_code"`
	Started  time.Time `json:"started_at"`
	Ended    time.Time `json:"ended_at,omitempty"`
	PID      int       `json:"pid"`
	Error    string    `json:"error,omitempty"`
	Buffered int64     `json:"buffered_bytes"`
}

func (t *Task) snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Snapshot{
		ID:       t.ID,
		Label:    t.Label,
		Status:   t.Status,
		ExitCode: t.ExitCode,
		Started:  t.Started,
		Ended:    t.Ended,
		PID:      t.PID,
		Error:    t.err,
		Buffered: t.buf.Size(),
	}
}

// Manager 持有全部任务，进程重启即清空。
type Manager struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	order []string
	limit int
}

// NewManager 创建任务管理器，limit 为同时保留的最大任务数。
func NewManager(limit int) *Manager {
	if limit <= 0 {
		limit = 64
	}
	return &Manager{tasks: make(map[string]*Task), limit: limit}
}

// Start 启动子进程并把输出接入环形缓冲区。
func (m *Manager) Start(cmd *exec.Cmd, label string) (*Task, error) {
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

	t := &Task{
		ID:      newID(),
		Label:   label,
		Status:  StatusRunning,
		Started: time.Now(),
		PID:     cmd.Process.Pid,
		buf:     NewRingBuffer(1 << 20),
		cmd:     cmd,
		done:    make(chan struct{}),
	}

	go t.pump(stdout)
	go t.pump(stderr)
	go t.wait(cmd)

	m.add(t)
	return t, nil
}

// Get 按 ID 查找任务。
func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	return t, ok
}

// List 返回全部任务的快照，最近的在前。
func (m *Manager) List() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Snapshot, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if t, ok := m.tasks[m.order[i]]; ok {
			out = append(out, t.snapshot())
		}
	}
	return out
}

func (m *Manager) add(t *Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)

	// 超出上限时淘汰最老且已结束的任务
	for len(m.order) > m.limit {
		oldest := m.order[0]
		if ot, ok := m.tasks[oldest]; ok {
			if ot.snapshot().Status == StatusRunning {
				break
			}
			delete(m.tasks, oldest)
		}
		m.order = m.order[1:]
	}
}

func (t *Task) pump(r io.Reader) {
	dec := &winenc.LineDecoder{}
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			for _, line := range dec.Feed(buf[:n]) {
				t.buf.WriteString(line + "\n")
			}
		}
		if err != nil {
			break
		}
	}
	for _, line := range dec.Flush() {
		t.buf.WriteString(line + "\n")
	}
}

func (t *Task) wait(cmd *exec.Cmd) {
	err := cmd.Wait()

	t.mu.Lock()
	t.Ended = time.Now()
	if err == nil {
		t.Status = StatusExited
		t.ExitCode = 0
	} else if ee, ok := err.(*exec.ExitError); ok {
		t.Status = StatusExited
		t.ExitCode = ee.ExitCode()
	} else if t.Status == StatusKilled {
		t.ExitCode = -1
	} else {
		t.Status = StatusFailed
		t.err = err.Error()
		t.ExitCode = -1
	}
	t.mu.Unlock()

	close(t.done)
}

// Read 增量读取输出。offset 传上次返回的 next 值，首次传 0。
func (t *Task) Read(offset int64) (data string, next int64, truncated bool) {
	s, n, tr := t.buf.ReadFrom(offset)
	return s, n, tr
}

// Kill 终止任务及其子进程树。
func (t *Task) Kill() error {
	t.mu.Lock()
	if t.Status != StatusRunning {
		t.mu.Unlock()
		return fmt.Errorf("任务 %s 已结束", t.ID)
	}
	t.Status = StatusKilled
	pid := t.PID
	t.mu.Unlock()

	// Windows 上 Kill 只杀直接子进程，脚本派生出的进程会变成孤儿，用 taskkill /T 兜底。
	if err := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(pid)).Run(); err != nil {
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
	}
	return nil
}

// Wait 阻塞直到任务结束或超时，返回是否已结束。
func (t *Task) Wait(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "t_" + hex.EncodeToString(b)
}
