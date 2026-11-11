package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"juyuagent/internal/rpc"
)

func cmdHealth(ctx context.Context, c *Client) error {
	m, err := c.Health(ctx)
	if err != nil {
		return fmt.Errorf("无法连接 agentd: %w", err)
	}
	if *flagJSON {
		emitJSON(m)
		return nil
	}
	fmt.Printf("agentd %v 在线\n", m["version"])
	fmt.Printf("主机: %v   已运行: %v 秒   可访问根目录: %v 个\n", m["host"], fmtNum(m["uptime"]), fmtNum(m["roots"]))
	return nil
}

func cmdTools(ctx context.Context, c *Client) error {
	tools, err := c.Tools(ctx)
	if err != nil {
		return err
	}
	if *flagJSON {
		emitJSON(tools)
		return nil
	}
	fmt.Printf("共 %d 个可用工具\n\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  %-14s %s\n", t.Name, t.Description)
	}
	return nil
}

func cmdLs(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "ls <path>"); err != nil {
		return err
	}
	raw, rerr := c.Call(ctx, "fs.list", map[string]interface{}{
		"path":        rest[0],
		"show_hidden": hasFlag(flags, "a", "all"),
	})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Path      string `json:"path"`
		Count     int    `json:"count"`
		Truncated bool   `json:"truncated"`
		Entries   []struct {
			Name    string    `json:"name"`
			IsDir   bool      `json:"is_dir"`
			Size    int64     `json:"size"`
			ModTime time.Time `json:"mod_time"`
		} `json:"entries"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}

	fmt.Printf("%s\n", res.Path)
	rows := make([][]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		kind := "文件"
		size := humanSize(e.Size)
		if e.IsDir {
			kind = "目录"
			size = "-"
		}
		rows = append(rows, []string{kind, size, e.ModTime.Format("2006-01-02 15:04"), e.Name})
	}
	printTable([]string{"类型", "大小", "修改时间", "名称"}, rows)
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "\n[条目过多，已截断]")
	}
	return nil
}

func cmdCat(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 1, "cat <path> [offset] [limit]"); err != nil {
		return err
	}
	params := map[string]interface{}{"path": rest[0]}
	if len(rest) > 1 {
		params["offset"] = atoi(rest[1])
	}
	if len(rest) > 2 {
		params["limit"] = atoi(rest[2])
	}

	raw, rerr := c.Call(ctx, "fs.read", params)
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Content    string `json:"content"`
		Truncated  bool   `json:"truncated"`
		NextOffset int    `json:"next_offset"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}
	fmt.Print(res.Content)
	if res.Content != "" && !strings.HasSuffix(res.Content, "\n") {
		fmt.Println()
	}
	if res.Truncated {
		// 提示走 stderr，保证管道里拿到的仍是纯文件内容
		fmt.Fprintf(os.Stderr, "\n[内容未读完，继续: -offset %d]\n", res.NextOffset)
	}
	return nil
}

func cmdRead(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 1, "read <path>"); err != nil {
		return err
	}
	raw, rerr := c.Call(ctx, "fs.read", map[string]interface{}{"path": rest[0]})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Path       string `json:"path"`
		Size       int64  `json:"size"`
		Lines      int    `json:"lines"`
		Truncated  bool   `json:"truncated"`
		NextOffset int    `json:"next_offset"`
		Content    string `json:"content"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}

	fmt.Printf("路径: %s\n大小: %s   行数: %d   截断: %v   下个偏移: %d\n",
		res.Path, humanSize(res.Size), res.Lines, res.Truncated, res.NextOffset)
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(res.Content)
	return nil
}

func cmdWrite(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "write <path> [内容|-]"); err != nil {
		return err
	}

	content, err := readContentArg(rest[1:], flags)
	if err != nil {
		return err
	}

	raw, rerr := c.Call(ctx, "fs.write", map[string]interface{}{
		"path":    rest[0],
		"content": content,
	})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}
	var res struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}
	_ = unwrap(raw, &res)
	fmt.Printf("已写入 %s (%d 字节)\n", res.Path, res.Bytes)
	return nil
}

func cmdAppend(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "append <path> [内容|-]"); err != nil {
		return err
	}
	content, err := readContentArg(rest[1:], flags)
	if err != nil {
		return err
	}

	raw, rerr := c.Call(ctx, "fs.append", map[string]interface{}{
		"path":    rest[0],
		"content": content,
	})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}
	var res struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}
	_ = unwrap(raw, &res)
	fmt.Printf("已追加 %d 字节到 %s\n", res.Bytes, res.Path)
	return nil
}

// push 把本地文件传到电脑 C，分块走 fs.write / fs.append，绕开单请求体积上限。
func cmdPush(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 2, "push <本地路径> <远程路径>"); err != nil {
		return err
	}

	data, err := os.ReadFile(rest[0])
	if err != nil {
		return fmt.Errorf("读取本地文件失败: %w", err)
	}

	const chunk = 3 << 20
	total := 0
	for offset := 0; offset < len(data) || offset == 0; offset += chunk {
		end := offset + chunk
		if end > len(data) {
			end = len(data)
		}
		part := base64.StdEncoding.EncodeToString(data[offset:end])

		method := "fs.append"
		params := map[string]interface{}{
			"path":     rest[1],
			"content":  part,
			"encoding": "base64",
		}
		if offset == 0 {
			method = "fs.write"
		}
		if _, rerr := c.Call(ctx, method, params); rerr != nil {
			return fmt.Errorf("传输到第 %d 字节失败: %w", offset, rerr)
		}
		total += end - offset
		if end == len(data) {
			break
		}
	}

	fmt.Printf("已上传 %s -> %s (%s)\n", rest[0], rest[1], humanSize(int64(total)))
	return nil
}

// pull 把电脑 C 上的文件拉回本地，按字节偏移循环读取。
func cmdPull(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 2, "pull <远程路径> <本地路径>"); err != nil {
		return err
	}

	var buf []byte
	offset := 0
	for {
		raw, rerr := c.Call(ctx, "fs.read", map[string]interface{}{
			"path":     rest[0],
			"offset":   offset,
			"limit":    3 << 20,
			"encoding": "base64",
		})
		if rerr != nil {
			return rerr
		}
		var res struct {
			Content    string `json:"content"`
			Truncated  bool   `json:"truncated"`
			NextOffset int    `json:"next_offset"`
		}
		if err := unwrap(raw, &res); err != nil {
			return err
		}

		part, err := base64.StdEncoding.DecodeString(res.Content)
		if err != nil {
			return fmt.Errorf("解码失败: %w", err)
		}
		buf = append(buf, part...)
		if !res.Truncated || res.NextOffset == offset {
			break
		}
		offset = res.NextOffset
	}

	if dir := filepath.Dir(rest[1]); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(rest[1], buf, 0o644); err != nil {
		return fmt.Errorf("写本地文件失败: %w", err)
	}

	fmt.Printf("已下载 %s -> %s (%s)\n", rest[0], rest[1], humanSize(int64(len(buf))))
	return nil
}

// readContentArg 统一处理三种内容来源：命令行参数、-f 指定文件、stdin 管道。
func readContentArg(rest []string, flags map[string]string) (string, error) {
	if file := flagValue(flags, "f", "file"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("读取源文件失败: %w", err)
		}
		return string(data), nil
	}
	if len(rest) > 0 && rest[0] != "-" {
		return rest[0], nil
	}
	if len(rest) > 0 || stdinHasData() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("读取 stdin 失败: %w", err)
		}
		return string(data), nil
	}
	return "", nil
}

func cmdRm(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "rm <path> [-r]"); err != nil {
		return err
	}
	raw, rerr := c.Call(ctx, "fs.remove", map[string]interface{}{
		"path":      rest[0],
		"recursive": hasFlag(flags, "r", "recursive"),
	})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}
	fmt.Printf("已删除 %s\n", rest[0])
	return nil
}

func cmdMkdir(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 1, "mkdir <path>"); err != nil {
		return err
	}
	if _, rerr := c.Call(ctx, "fs.mkdir", map[string]interface{}{"path": rest[0]}); rerr != nil {
		return rerr
	}
	fmt.Printf("已创建 %s\n", rest[0])
	return nil
}

func cmdMove(ctx context.Context, c *Client, args []string) error {
	return cmdTwoPath(ctx, c, "fs.move", "移动", args)
}

func cmdCopy(ctx context.Context, c *Client, args []string) error {
	return cmdTwoPath(ctx, c, "fs.copy", "复制", args)
}

func cmdTwoPath(ctx context.Context, c *Client, method, label string, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 2, "需要源路径和目标路径"); err != nil {
		return err
	}
	raw, rerr := c.Call(ctx, method, map[string]interface{}{
		"src":       rest[0],
		"dst":       rest[1],
		"overwrite": hasFlag(flags, "f", "force"),
	})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}
	fmt.Printf("已%s %s -> %s\n", label, rest[0], rest[1])
	return nil
}

func cmdFind(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "find <root> [pattern] [-c 关键字]"); err != nil {
		return err
	}
	params := map[string]interface{}{"root": rest[0]}
	if len(rest) > 1 {
		params["name_pattern"] = rest[1]
	}
	if kw := flagValue(flags, "c", "content"); kw != "" {
		params["content"] = kw
	}

	raw, rerr := c.Call(ctx, "fs.search", params)
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Count     int  `json:"count"`
		Truncated bool `json:"truncated"`
		Hits      []struct {
			Path    string `json:"path"`
			Size    int64  `json:"size"`
			Matched string `json:"matched_by"`
		} `json:"hits"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}

	fmt.Printf("命中 %d 个\n\n", res.Count)
	for _, h := range res.Hits {
		fmt.Printf("  %-10s %-9s %s\n", h.Matched, humanSize(h.Size), h.Path)
	}
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "\n[结果已截断，可缩小搜索范围]")
	}
	return nil
}

func cmdExec(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "exec <命令...>"); err != nil {
		return err
	}
	params := map[string]interface{}{"command": strings.Join(rest, " ")}
	if v := flagValue(flags, "timeout"); v != "" {
		params["timeout"] = atoi(v)
	}
	return runExec(ctx, c, "exec.run", params)
}

func cmdSpawn(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 1, "spawn <命令...>"); err != nil {
		return err
	}
	raw, rerr := c.Call(ctx, "exec.spawn", map[string]interface{}{
		"command": strings.Join(rest, " "),
	})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}
	var res struct {
		ID  string `json:"id"`
		PID int    `json:"pid"`
	}
	_ = unwrap(raw, &res)
	fmt.Printf("已启动任务 %s (PID %d)\n", res.ID, res.PID)
	fmt.Printf("查看输出: agentctl log %s\n", res.ID)
	return nil
}

// runExec 执行并处理流式输出与退出码。
func runExec(ctx context.Context, c *Client, method string, params map[string]interface{}) error {
	var raw json.RawMessage
	var rerr *rpc.Error

	if *flagNoStream {
		raw, rerr = c.Call(ctx, method, params)
	} else {
		raw, rerr = c.CallStream(ctx, method, params, func(kind, chunk string) {
			if kind == "stderr" {
				fmt.Fprint(os.Stderr, chunk)
			} else {
				fmt.Fprint(os.Stdout, chunk)
			}
		})
	}
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		emitJSON(json.RawMessage(raw))
		return nil
	}

	var res struct {
		ExitCode   int    `json:"exit_code"`
		TimedOut   bool   `json:"timed_out"`
		DurationMS int64  `json:"duration_ms"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
	}
	_ = unwrap(raw, &res)

	if *flagNoStream {
		if res.Stdout != "" {
			fmt.Print(res.Stdout)
			if !strings.HasSuffix(res.Stdout, "\n") {
				fmt.Println()
			}
		}
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
			if !strings.HasSuffix(res.Stderr, "\n") {
				fmt.Fprintln(os.Stderr)
			}
		}
	}

	if res.TimedOut {
		fmt.Fprintf(os.Stderr, "\n[执行超时，已强制结束]\n")
		os.Exit(124)
	}
	if res.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "\n[退出码 %d，耗时 %d ms]\n", res.ExitCode, res.DurationMS)
		os.Exit(res.ExitCode)
	}
	return nil
}

func cmdTasks(ctx context.Context, c *Client) error {
	raw, rerr := c.Call(ctx, "task.list", nil)
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Tasks []struct {
			ID       string    `json:"id"`
			Label    string    `json:"label"`
			Status   string    `json:"status"`
			ExitCode int       `json:"exit_code"`
			PID      int       `json:"pid"`
			Started  time.Time `json:"started_at"`
		} `json:"tasks"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}
	if len(res.Tasks) == 0 {
		fmt.Println("暂无后台任务")
		return nil
	}

	rows := make([][]string, 0, len(res.Tasks))
	for _, t := range res.Tasks {
		rows = append(rows, []string{
			t.ID, t.Status, fmt.Sprintf("%d", t.ExitCode),
			fmt.Sprintf("%d", t.PID), t.Started.Format("15:04:05"), truncate(t.Label, 40),
		})
	}
	printTable([]string{"ID", "状态", "退出码", "PID", "启动时间", "命令"}, rows)
	return nil
}

func cmdLog(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 1, "log <id> [offset]"); err != nil {
		return err
	}
	offset := int64(0)
	if len(rest) > 1 {
		offset = int64(atoi(rest[1]))
	}

	raw, rerr := c.Call(ctx, "task.log", map[string]interface{}{"id": rest[0], "offset": offset})
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Output     string `json:"output"`
		NextOffset int64  `json:"next_offset"`
		Status     string `json:"status"`
		Truncated  bool   `json:"truncated"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}

	fmt.Print(res.Output)
	if res.Output != "" && !strings.HasSuffix(res.Output, "\n") {
		fmt.Println()
	}
	fmt.Fprintf(os.Stderr, "\n[状态 %s，下次 offset=%d]\n", res.Status, res.NextOffset)
	return nil
}

func cmdKill(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	if err := needArgs(rest, 1, "kill <id>"); err != nil {
		return err
	}
	if _, rerr := c.Call(ctx, "task.kill", map[string]interface{}{"id": rest[0]}); rerr != nil {
		return rerr
	}
	fmt.Printf("已终止 %s\n", rest[0])
	return nil
}

func cmdPy(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	code := ""
	if file := flagValue(flags, "f", "file"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("读取脚本失败: %w", err)
		}
		code = string(data)
	} else {
		if err := needArgs(rest, 1, "py <代码> 或 py -f <文件>"); err != nil {
			return err
		}
		code = strings.Join(rest, " ")
	}

	params := map[string]interface{}{"code": code}
	if v := flagValue(flags, "timeout"); v != "" {
		params["timeout"] = atoi(v)
	}
	return runExec(ctx, c, "python.exec", params)
}

func cmdRepl(ctx context.Context, c *Client, args []string) error {
	flags, rest := splitFlags(args)
	if err := needArgs(rest, 1, "repl <代码> [-s 会话名]"); err != nil {
		return err
	}
	params := map[string]interface{}{
		"code":    strings.Join(rest, " "),
		"session": flagValue(flags, "s", "session"),
		"reset":   hasFlag(flags, "reset", "r"),
	}
	if v := flagValue(flags, "timeout"); v != "" {
		params["timeout"] = atoi(v)
	}

	raw, rerr := c.Call(ctx, "python.repl", params)
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Output  string `json:"output"`
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Session string `json:"session"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}
	if res.Output != "" {
		fmt.Print(res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			fmt.Println()
		}
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, res.Error)
		return fmt.Errorf("执行失败（会话 %s）", res.Session)
	}
	return nil
}

func cmdInfo(ctx context.Context, c *Client) error {
	raw, rerr := c.Call(ctx, "sys.info", nil)
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res map[string]interface{}
	if err := unwrap(raw, &res); err != nil {
		return err
	}

	fmt.Printf("主机名     %v\n", res["hostname"])
	fmt.Printf("系统       %v/%v  %v 核\n", res["os"], res["arch"], fmtNum(res["cpus"]))
	fmt.Printf("agentd     %v（已运行 %v 秒）\n", res["agent_version"], fmtNum(res["uptime_sec"]))
	fmt.Printf("工作目录   %v\n", res["work_dir"])

	if d, ok := res["disks"].([]interface{}); ok && len(d) > 0 {
		fmt.Println("磁盘:")
		for _, item := range d {
			dm, _ := item.(map[string]interface{})
			fmt.Printf("  %-6v 总 %s   可用 %s   已用 %.0f%%\n",
				dm["drive"], humanSize(int64(fmtFloat(dm["total_bytes"]))),
				humanSize(int64(fmtFloat(dm["free_bytes"]))), fmtFloat(dm["used_percent"]))
		}
	}
	if m, ok := res["memory"].(map[string]interface{}); ok {
		fmt.Printf("内存       总 %s   可用 %s   已用 %.0f%%\n",
			humanSize(int64(fmtFloat(m["total_bytes"]))),
			humanSize(int64(fmtFloat(m["avail_bytes"]))), fmtFloat(m["used_percent"]))
	}
	if p, ok := res["python"].(map[string]interface{}); ok {
		fmt.Printf("Python     %v (%v)\n", p["version"], p["path"])
	}
	return nil
}

func cmdPs(ctx context.Context, c *Client, args []string) error {
	_, rest := splitFlags(args)
	params := map[string]interface{}{"limit": 200}
	if len(rest) > 0 {
		params["filter"] = rest[0]
	}

	raw, rerr := c.Call(ctx, "sys.procs", params)
	if rerr != nil {
		return rerr
	}
	if *flagJSON {
		printRaw(raw)
		return nil
	}

	var res struct {
		Count     int `json:"count"`
		Processes []struct {
			Name string `json:"name"`
			PID  int    `json:"pid"`
			Mem  string `json:"mem"`
		} `json:"processes"`
	}
	if err := unwrap(raw, &res); err != nil {
		return err
	}

	rows := make([][]string, 0, len(res.Processes))
	for _, p := range res.Processes {
		rows = append(rows, []string{fmt.Sprintf("%d", p.PID), p.Mem, p.Name})
	}
	printTable([]string{"PID", "内存", "进程名"}, rows)
	fmt.Fprintf(os.Stderr, "共 %d 个进程\n", res.Count)
	return nil
}

func cmdRaw(ctx context.Context, c *Client, args []string) error {
	if err := needArgs(args, 1, "rpc <method> [json参数]"); err != nil {
		return err
	}
	var params interface{}
	if len(args) > 1 {
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &params); err != nil {
			return fmt.Errorf("参数不是合法 JSON: %w", err)
		}
	}
	raw, rerr := c.Call(ctx, args[0], params)
	if rerr != nil {
		if *flagJSON {
			emitJSON(rerr)
		}
		return rerr
	}
	printRaw(raw)
	return nil
}

// ---------- 输出辅助 ----------

func printRaw(raw json.RawMessage) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		fmt.Println(string(raw))
		return
	}
	emitJSON(v)
}

func printTable(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if i < len(widths) {
				if w := displayWidth(cell); w > widths[i] {
					widths[i] = w
				}
			}
		}
	}

	var b strings.Builder
	for i, h := range headers {
		b.WriteString(padRight(h, widths[i]))
		b.WriteString("  ")
	}
	fmt.Println(strings.TrimRight(b.String(), " "))

	for _, r := range rows {
		b.Reset()
		for i, cell := range r {
			if i < len(widths) {
				b.WriteString(padRight(cell, widths[i]))
				b.WriteString("  ")
			}
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
}

// displayWidth 按东亚字符宽度计算列宽，否则中文文件名会把表格顶歪。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWide(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		(r >= 0x3000 && r <= 0x303F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6)
}

func padRight(s string, width int) string {
	gap := width - displayWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func fmtNum(v interface{}) int64 {
	return int64(fmtFloat(v))
}

func fmtFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}

// stdinHasData 判断 stdin 是否被管道或重定向接入。
func stdinHasData() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice == 0
}
