// Command PGCact 是部署在被控端（Windows 7）上的被控端。
//
// 它在局域网内监听一个端口，把文件读写、命令执行、Python 执行等能力
// 以 JSON-RPC 2.0 的形式暴露给控制端 上的 PGActl 或 MCP 适配器。
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"powergo/internal/config"
	"powergo/internal/discovery"
	"powergo/internal/server"
)

func main() {
	var (
		cfgPath = flag.String("config", "", "配置文件路径，默认与程序同目录的 pgcact.json")
		showVer = flag.Bool("version", false, "打印版本号后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("PowerGo PGCact %s\n", server.Version)
		return
	}

	if *cfgPath == "" {
		*cfgPath = defaultConfigPath()
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("加载配置失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		fatal("配置校验失败: %v", err)
	}

	setupLogging(cfg.LogFile)

	srv := server.New(cfg)
	srv.Addr = cfg.Listen
	srv.CfgPath = *cfgPath
	defer srv.Close()

	httpPort := portOfInt(cfg.Listen)

	// 启动 UDP 广播发现，让客户端无需知道本机 IP 就能找到服务
	if discPort := cfg.DiscoveryPortOr(); discPort > 0 {
		go func() {
			host, _ := os.Hostname()
			err := discovery.Serve(discPort, func(probe *discovery.Packet, remoteIP string) *discovery.Packet {
				return &discovery.Packet{
					NodeID:       cfg.NodeID,
					Host:         host,
					HTTPPort:     httpPort,
					AgentVersion: server.Version,
					Pairing:      cfg.PairingOn(),
				}
			})
			if err != nil {
				log.Printf("UDP 发现服务启动失败（端口 %d）: %v。客户端将无法自动发现，需手动指定 -addr", discPort, err)
			}
		}()
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：流式输出和长任务会被它直接掐断
	}

	banner(cfg, *cfgPath)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("监听 %s 失败: %v", cfg.Listen, err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("正在关闭...")
	srv.Close()
	_ = httpServer.Close()
	log.Println("已退出")
}

func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "pgcact.json"
	}
	dir := filepath.Dir(exe)
	primary := filepath.Join(dir, "pgcact.json")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	// 兼容 0.x 版本的配置文件名，避免升级后重新生成 Token 导致凭据失效
	legacy := filepath.Join(dir, "agentd.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return primary
}

func setupLogging(file string) {
	log.SetFlags(log.LstdFlags)
	if file == "" {
		return
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("日志文件打开失败，仅输出到控制台: %v", err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
}

func banner(cfg *config.Config, cfgPath string) {
	localIP := firstIPv4()
	httpPort := portOfInt(cfg.Listen)
	sep := "  ------------------------------------------------------------"

	fmt.Printf("\n  PowerGo (抛二钩) PGCact v%s\n%s\n", server.Version, sep)
	fmt.Printf("  配置文件    %s\n", cfgPath)
	fmt.Printf("  监听地址    %s\n", cfg.Listen)
	fmt.Printf("  本机 IP     %s\n", localIP)
	fmt.Printf("  节点 ID     %s\n", cfg.NodeID)
	fmt.Printf("  Token       %s\n", cfg.Token)
	fmt.Printf("  允许根目录  %v\n", cfg.AllowedRoots)
	fmt.Println(sep)

	if cfg.PairingOn() {
		fmt.Printf("  配对码      %s\n", cfg.PairCode)
		fmt.Printf("  在控制端执行 PGActl pair，输入上面这 6 位数字即可接入\n")
		fmt.Printf("  （配对成功后会自动换成新码，旧码立即失效）\n")
	} else {
		fmt.Printf("  配对已关闭，客户端需手动配置 Token\n")
	}

	if cfg.DiscoveryPortOr() > 0 {
		fmt.Printf("  广播发现    UDP %d 已开启\n", cfg.DiscoveryPortOr())
	} else {
		fmt.Printf("  广播发现    已关闭\n")
	}
	fmt.Println(sep)

	fmt.Printf("  控制端只需执行：PGActl info\n")
	fmt.Printf("  手动指定地址：PGActl -addr http://%s:%d info\n\n",
		localIP, httpPort)

	if cfg.GeneratedToken {
		fmt.Println("  已生成新 Token 并写入配置文件，请妥善保存")
		fmt.Println()
	}
}

// portOfInt 从监听地址里取出端口号，供发现服务广播使用。
func portOfInt(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return 7777
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 7777
	}
	return n
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "7777"
	}
	return port
}

// firstIPv4 选一个控制端大概率能连通的地址用于提示。
//
// 不能简单取第一个非回环地址：装有 VMware / Hyper-V / 蓝牙等虚拟网卡的机器，
// 枚举顺序靠前的往往是 169.254.x.x 这类 DHCP 失败时的自动私有地址，照它填必然连不上。
// 策略是优先私有网段，并明确排除 169.254/16。
func firstIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}

	fallback := ""
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		if ip4[0] == 169 && ip4[1] == 254 {
			continue
		}
		if isPrivateIPv4(ip4) {
			return ip4.String()
		}
		if fallback == "" {
			fallback = ip4.String()
		}
	}
	if fallback != "" {
		return fallback
	}
	return "127.0.0.1"
}

// isPrivateIPv4 判断是否为 RFC1918 私有地址（10/8、172.16/12、192.168/16）。
func isPrivateIPv4(ip net.IP) bool {
	switch {
	case ip[0] == 10:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return true
	}
	return false
}

func fatal(format string, args ...interface{}) {
	log.Printf(format, args...)
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
