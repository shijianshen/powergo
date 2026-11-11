// Command agentd 是部署在电脑 C（Windows 7）上的被控端。
//
// 它在局域网内监听一个端口，把文件读写、命令执行、Python 执行等能力
// 以 JSON-RPC 2.0 的形式暴露给电脑 A 上的 agentctl 或 MCP 适配器。
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

	"juyuagent/internal/config"
	"juyuagent/internal/discovery"
	"juyuagent/internal/server"
)

func main() {
	var (
		cfgPath = flag.String("config", "", "配置文件路径，默认与程序同目录的 agentd.json")
		showVer = flag.Bool("version", false, "打印版本号后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("juyu agentd %s\n", server.Version)
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
		return "agentd.json"
	}
	return filepath.Join(filepath.Dir(exe), "agentd.json")
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

	fmt.Printf("\n  juyu agentd %s\n%s\n", server.Version, sep)
	fmt.Printf("  配置文件    %s\n", cfgPath)
	fmt.Printf("  监听地址    %s\n", cfg.Listen)
	fmt.Printf("  本机 IP     %s\n", localIP)
	fmt.Printf("  节点 ID     %s\n", cfg.NodeID)
	fmt.Printf("  Token       %s\n", cfg.Token)
	fmt.Printf("  允许根目录  %v\n", cfg.AllowedRoots)
	fmt.Println(sep)

	if cfg.PairingOn() {
		fmt.Printf("  配对码      %s\n", cfg.PairCode)
		fmt.Printf("  在电脑 A 上执行 agentctl pair，输入上面这 6 位数字即可接入\n")
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

	fmt.Printf("  在电脑 A 上只需执行：agentctl info\n")
	fmt.Printf("  手动指定地址：agentctl -addr http://%s:%d info\n\n",
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

// firstIPv4 找第一个非回环的 IPv4 地址，用于提示 A 端填哪个地址。
func firstIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return "127.0.0.1"
}

func fatal(format string, args ...interface{}) {
	log.Printf(format, args...)
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
