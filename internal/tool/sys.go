package tool

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"powergo/internal/rpc"
	"powergo/internal/winenc"
)

// RegisterSys 注册系统信息能力。
func RegisterSys(r *Registry) {
	r.Register(&Tool{
		Name:        "sys.info",
		Description: "获取主机名、系统版本、CPU/内存、磁盘容量以及 agent 自身的运行状态。",
		Schema:      BuildSchema(nil),
		Handler:     sysInfo,
	})

	r.Register(&Tool{
		Name:        "sys.procs",
		Description: "列出正在运行的进程。",
		Schema: BuildSchema([]Param{
			{Name: "filter", Type: "string", Desc: "按进程名模糊过滤，如 chrome"},
			{Name: "limit", Type: "integer", Desc: "最多返回条数", Default: 100},
		}),
		Handler: sysProcs,
	})

	r.Register(&Tool{
		Name:        "sys.env",
		Description: "读取环境变量。传 key 取单个值，不传返回全部。",
		Schema: BuildSchema([]Param{
			{Name: "key", Type: "string", Desc: "变量名"},
		}),
		Handler: sysEnv,
	})
}

func sysInfo(c *Context, _ json.RawMessage) (interface{}, *rpc.Error) {
	host, _ := os.Hostname()
	wd, _ := os.Getwd()

	info := map[string]interface{}{
		"hostname":      host,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"cpus":          runtime.NumCPU(),
		"go_version":    runtime.Version(),
		"agent_version": c.Version,
		"work_dir":      wd,
		"config_dir":    c.Cfg.WorkDir,
		"allowed_roots": c.Box.Roots(),
		"uptime_sec":    int64(time.Since(c.Started).Seconds()),
		"now":           time.Now(),
	}

	if u, err := os.UserHomeDir(); err == nil {
		info["home"] = u
	}
	if mem, err := memoryStatus(); err == nil {
		info["memory"] = mem
	}
	if disks, err := diskUsage(c); err == nil {
		info["disks"] = disks
	}
	if py := findPython(c); py.OK {
		info["python"] = py
	}
	return info, nil
}

type procEntry struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	Mem  string `json:"mem,omitempty"`
}

func sysProcs(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Filter string `json:"filter"`
		Limit  int    `json:"limit"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	if p.Limit <= 0 {
		p.Limit = 100
	}

	cmd := exec.Command("tasklist", "/FO", "CSV", "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternalError, "枚举进程失败: %v", err)
	}

	// tasklist 输出是本地代码页（中文系统为 GBK），先转 UTF-8 再解析
	text := winenc.Decode(out)
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	rows, err := reader.ReadAll()
	if err != nil && len(rows) == 0 {
		return nil, rpc.NewError(rpc.CodeInternalError, "解析进程列表失败: %v", err)
	}

	procs := make([]procEntry, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		name := row[0]
		if p.Filter != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(p.Filter)) {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(row[1]))
		e := procEntry{Name: name, PID: pid}
		if len(row) > 4 {
			e.Mem = row[4]
		}
		procs = append(procs, e)
		if len(procs) >= p.Limit {
			break
		}
	}
	return map[string]interface{}{"count": len(procs), "processes": procs}, nil
}

func sysEnv(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Key string `json:"key"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	if p.Key == "" {
		vars := make(map[string]string, 64)
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i > 0 {
				vars[kv[:i]] = kv[i+1:]
			}
		}
		return map[string]interface{}{"count": len(vars), "vars": vars}, nil
	}
	v, ok := os.LookupEnv(p.Key)
	if !ok {
		return nil, rpc.NewError(rpc.CodeNotFound, "环境变量不存在: %s", p.Key)
	}
	return map[string]interface{}{"key": p.Key, "value": v}, nil
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type memoryInfo struct {
	TotalBytes uint64  `json:"total_bytes"`
	AvailBytes uint64  `json:"avail_bytes"`
	UsedPct    float64 `json:"used_percent"`
}

// memoryStatus 调用 GlobalMemoryStatusEx，Win7 起可用。
func memoryStatus() (memoryInfo, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return memoryInfo{}, err
	}
	return memoryInfo{
		TotalBytes: m.TotalPhys,
		AvailBytes: m.AvailPhys,
		UsedPct:    float64(m.MemoryLoad),
	}, nil
}

type diskInfo struct {
	Drive      string  `json:"drive"`
	TotalBytes uint64  `json:"total_bytes"`
	FreeBytes  uint64  `json:"free_bytes"`
	UsedPct    float64 `json:"used_percent"`
}

func diskUsage(c *Context) ([]diskInfo, error) {
	var out []diskInfo
	for _, root := range c.Box.Roots() {
		p, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		var freeAvail, total, freeTotal uint64
		r, _, _ := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&freeAvail)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&freeTotal)),
		)
		if r == 0 || total == 0 {
			continue
		}
		out = append(out, diskInfo{
			Drive:      root,
			TotalBytes: total,
			FreeBytes:  freeTotal,
			UsedPct:    float64(total-freeTotal) / float64(total) * 100,
		})
	}
	return out, nil
}
