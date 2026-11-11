// Command agentctl 是跑在电脑 A 上的命令行客户端，用于操作电脑 C 上的 agentd。
//
// 首次使用只需 `agentctl pair` 输入一次配对码，之后所有命令都会自动发现节点、
// 自动带凭据，电脑 C 换 IP 也能自己找回来。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	flagAddr       = flag.String("addr", "", "手动指定 agentd 地址，如 http://192.168.2.11:7777（跳过自动发现）")
	flagToken      = flag.String("token", "", "手动指定 Token（跳过配对）")
	flagJSON       = flag.Bool("json", false, "输出原始 JSON")
	flagNoStream   = flag.Bool("no-stream", false, "关闭流式输出，等命令结束一次性返回")
	flagTimeout    = flag.Int("timeout", 0, "请求超时秒数，0 表示不限")
	flagDiscPort   = flag.Int("disc-port", 7778, "UDP 发现端口")
	flagNoDiscover = flag.Bool("no-discover", false, "禁用广播发现，只用本地凭据或 -addr")
)

func main() {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	cmd, rest := args[0], args[1:]

	// 本地管理命令不需要连服务端
	if handled, err := handleLocal(cmd, rest); handled {
		if err != nil {
			fail(err)
		}
		return
	}

	ctx := context.Background()
	if *flagTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*flagTimeout)*time.Second)
		defer cancel()
	}

	client, err := resolveClient(ctx)
	if err != nil {
		fail(err)
	}

	if err := dispatch(ctx, client, cmd, rest); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	os.Exit(1)
}

// handleLocal 处理不依赖服务端连接的子命令，返回 true 表示已处理。
func handleLocal(cmd string, args []string) (bool, error) {
	switch cmd {
	case "discover", "scan":
		return true, cmdDiscover()
	case "pair":
		return true, cmdPair(args)
	case "nodes":
		return true, cmdNodes()
	case "forget":
		return true, cmdForget(args)
	}
	return false, nil
}

func dispatch(ctx context.Context, c *Client, cmd string, args []string) error {
	switch cmd {
	case "health":
		return cmdHealth(ctx, c)
	case "tools":
		return cmdTools(ctx, c)
	case "ls", "dir":
		return cmdLs(ctx, c, args)
	case "cat":
		return cmdCat(ctx, c, args)
	case "read":
		return cmdRead(ctx, c, args)
	case "write":
		return cmdWrite(ctx, c, args)
	case "append":
		return cmdAppend(ctx, c, args)
	case "push":
		return cmdPush(ctx, c, args)
	case "pull":
		return cmdPull(ctx, c, args)
	case "rm", "del":
		return cmdRm(ctx, c, args)
	case "mkdir":
		return cmdMkdir(ctx, c, args)
	case "mv", "move":
		return cmdMove(ctx, c, args)
	case "cp", "copy":
		return cmdCopy(ctx, c, args)
	case "find":
		return cmdFind(ctx, c, args)
	case "exec", "sh":
		return cmdExec(ctx, c, args)
	case "spawn":
		return cmdSpawn(ctx, c, args)
	case "tasks":
		return cmdTasks(ctx, c)
	case "log":
		return cmdLog(ctx, c, args)
	case "kill":
		return cmdKill(ctx, c, args)
	case "py", "python":
		return cmdPy(ctx, c, args)
	case "repl", "pyrepl":
		return cmdRepl(ctx, c, args)
	case "info":
		return cmdInfo(ctx, c)
	case "ps":
		return cmdPs(ctx, c, args)
	case "rpc":
		return cmdRaw(ctx, c, args)
	default:
		return fmt.Errorf("未知命令 %q，执行 agentctl -h 查看用法", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `juyu agentctl —— 远程操作电脑 C 上的 agentd

首次使用:
  agentctl pair              自动发现局域网内的节点并配对（需要输入一次配对码）
  agentctl discover          只看有哪些节点在线，不做配对

日常使用（配对后无需任何参数）:
  agentctl info              系统信息、磁盘、内存、Python 版本
  agentctl ls   <path>       列目录
  agentctl exec <命令...>    执行命令，实时输出
  agentctl py   <代码>       执行 Python
  节点 IP 变化时会自动重新发现并更新地址

节点管理:
  agentctl nodes             查看已配对的节点
  agentctl forget <主机名>    删除某节点凭据
  agentctl pair <配对码>      重新配对默认节点

文件:
  ls <path> [-a]                  列目录
  cat <path> [offset] [limit]      读取内容（适合管道）
  read <path>                      内容 + 元信息
  write <path> [内容|-]            写入（- 表示从 stdin 读）
  append <path> [内容|-]           追加
  push <本地> <远程>               上传到电脑 C
  pull <远程> <本地>               从电脑 C 下载
  rm <path> [-r]                   删除
  mkdir <path>                     创建目录
  mv / cp <src> <dst> [-f]         移动 / 复制
  find <root> [pattern] [-c 关键字] 递归搜索

执行:
  exec <命令...>                   同步执行
  spawn <命令...>                  后台执行，返回任务 ID
  tasks / log <id> [offset] / kill <id>
  py <代码> | py -f <文件>
  repl <代码> [-s 会话名] [-reset]

系统:
  info / ps [过滤词] / health / tools
  rpc <method> [json]             直接调用任意方法

全局参数:
  -addr <url>      手动指定地址，跳过自动发现
  -token <token>   手动指定 Token，跳过配对
  -no-discover     禁用广播发现
  -disc-port       发现端口，默认 7778
  -json            输出原始 JSON
  -no-stream       关闭流式输出
  -timeout <秒>    请求超时

示例:
  agentctl pair
  agentctl info
  agentctl exec "ipconfig"
  agentctl py "print(1+1)"
  agentctl repl "x = 42"
  agentctl -addr http://192.168.2.11:7777 -token 5dfd... info
`)
}

// splitFlags 解析子命令自带的短参数，如 -r / -f / -s name / --key=value。
// 用标准 flag 包会因为参数位置不同而报错，这里手工处理更符合 CLI 直觉。
func splitFlags(args []string) (map[string]string, []string) {
	flags := make(map[string]string)
	rest := make([]string, 0, len(args))

	needsValue := map[string]bool{
		"f": true, "file": true, "s": true, "session": true,
		"c": true, "content": true, "timeout": true, "o": true, "offset": true,
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' || isNumeric(a) {
			rest = append(rest, a)
			continue
		}
		key := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(key, "="); ok {
			flags[k] = v
			continue
		}
		if needsValue[key] && i+1 < len(args) {
			flags[key] = args[i+1]
			i++
			continue
		}
		flags[key] = "true"
	}
	return flags, rest
}

func isNumeric(s string) bool {
	if s == "" || s == "-" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func hasFlag(flags map[string]string, keys ...string) bool {
	for _, k := range keys {
		if v, ok := flags[k]; ok && v != "false" {
			return true
		}
	}
	return false
}

func flagValue(flags map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := flags[k]; ok {
			return v
		}
	}
	return ""
}

func emitJSON(v interface{}) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Printf("%v\n", v)
		return
	}
	fmt.Println(string(b))
}

func needArgs(args []string, n int, hint string) error {
	if len(args) < n {
		return fmt.Errorf("参数不足：%s", hint)
	}
	return nil
}
