package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shijianshen/powergo/internal/discovery"
)

// pairReply 与服务端 /pair 的响应结构对应。
type pairReply struct {
	OK       bool   `json:"ok"`
	Locked   bool   `json:"locked,omitempty"`
	Error    string `json:"error,omitempty"`
	Token    string `json:"token,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	Host     string `json:"host,omitempty"`
	HTTPPort int    `json:"http_port,omitempty"`
	Version  string `json:"version,omitempty"`
}

// resolveClient 决定本次要连哪台 PGCact，并保证拿到可用的 Token。
//
// 优先级：
//  1. -addr 显式指定 —— 最高优先级，用于跨网段等发现不到的场景
//  2. 本地凭据里的默认节点 —— 直接连，失败则广播重找（自愈）
//  3. 广播发现 —— 单节点自动配对，多节点让用户选
func resolveClient(ctx context.Context) (*Client, error) {
	store := loadCreds()

	if *flagToken != "" && *flagAddr != "" {
		return NewClient(*flagAddr, *flagToken), nil
	}

	// 显式给了地址：复用凭据里的 Token，没有就走配对
	if *flagAddr != "" {
		addr := normalizeAddr(*flagAddr)
		if *flagToken != "" {
			return NewClient(addr, *flagToken), nil
		}
		if c, ok := store.FindByAddr(addr); ok {
			return NewClient(addr, c.Token), nil
		}
		reply, err := pairWith(addr, "", "")
		if err != nil {
			return nil, err
		}
		return NewClient(addr, reply.Token), nil
	}

	// 有已配对节点：先直连，连不上自动重新发现
	if c, ok := store.Current(); ok {
		client := NewClient(c.Addr, c.Token)
		if err := checkHealth(ctx, client); err == nil {
			store.Touch(c.NodeID, "")
			_ = store.Save()
			return client, nil
		}
		if *flagNoDiscover {
			return nil, fmt.Errorf("节点 %s (%s) 无响应，且已禁用自动发现", c.Host, c.Addr)
		}

		fmt.Fprintf(os.Stderr, "节点 %s 无响应，正在局域网内重新查找...\n", c.Host)
		peers, err := discovery.Probe(*flagDiscPort, 1200*time.Millisecond)
		if err != nil {
			return nil, fmt.Errorf("广播发现失败: %w", err)
		}
		for _, p := range peers {
			if p.NodeID == c.NodeID {
				store.Touch(p.NodeID, p.Addr())
				_ = store.Save()
				fmt.Fprintf(os.Stderr, "找到 %s，地址已更新为 %s\n", p.Host, p.Addr())
				return NewClient(p.Addr(), c.Token), nil
			}
		}
		return nil, fmt.Errorf("局域网内未找到 %s，请确认被控端 已开机且 PGCact 正在运行", c.Host)
	}

	// 从未配对过：广播发现
	if *flagNoDiscover {
		return nil, errors.New("没有可用凭据，且已禁用自动发现；请用 -addr 与 -token 指定")
	}

	fmt.Fprintln(os.Stderr, "正在局域网内搜索 PGCact...")
	peers, err := discovery.Probe(*flagDiscPort, 1200*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("广播发现失败: %w", err)
	}
	if len(peers) == 0 {
		return nil, errors.New("局域网内没有找到 PGCact。\n" +
			"请确认：1) 被控端 上的 PGCact 已启动  2) 两台机器在同一网段  " +
			"3) 被控端 的 UDP 7778 端口已放行")
	}

	peer := selectPeer(peers)

	// 该节点之前配对过（比如手动删了 Default），直接复用旧凭据
	if c, ok := store.Get(peer.NodeID); ok {
		client := NewClient(peer.Addr(), c.Token)
		if err := checkHealth(ctx, client); err == nil {
			store.Touch(peer.NodeID, peer.Addr())
			_ = store.Save()
			return client, nil
		}
	}

	reply, err := pairWith(peer.Addr(), peer.Host, "")
	if err != nil {
		return nil, err
	}

	nodeID := reply.NodeID
	if nodeID == "" {
		nodeID = peer.NodeID
	}
	host := reply.Host
	if host == "" {
		host = peer.Host
	}

	store.Put(credential{
		NodeID:   nodeID,
		Host:     host,
		Addr:     peer.Addr(),
		Token:    reply.Token,
		PairedAt: time.Now(),
		LastSeen: time.Now(),
	})
	if err := store.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 凭据保存失败，下次需重新配对: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "已保存凭据: %s\n", store.Path())
	}
	return NewClient(peer.Addr(), reply.Token), nil
}

// pairWith 交互式完成配对，最多允许输错 3 次配对码。
func pairWith(addr, host, presetCode string) (*pairReply, error) {
	if host == "" {
		host = addr
	}
	fmt.Fprintf(os.Stderr, "正在与 %s 配对\n", host)

	// 配对码作为参数传入时先直接试，失败再退回交互输入
	if presetCode != "" {
		reply, err := postPair(addr, presetCode)
		if err != nil {
			return nil, err
		}
		if reply.OK {
			fmt.Fprintf(os.Stderr, "配对成功，节点 %s\n", reply.Host)
			return reply, nil
		}
		if reply.Locked {
			return nil, errors.New(reply.Error)
		}
		fmt.Fprintf(os.Stderr, "配对码 %s 不正确: %s\n", presetCode, reply.Error)
		fmt.Fprintln(os.Stderr, "可以再手动输入一次，或按 Ctrl+C 退出")
	}

	for attempt := 1; attempt <= 3; attempt++ {
		code, err := promptLine("请输入被控端 控制台显示的 6 位配对码: ")
		if err != nil {
			return nil, errors.New("无法读取输入；可直接把配对码写在命令后面: PGAcli pair <配对码>")
		}
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}

		reply, err := postPair(addr, code)
		if err != nil {
			return nil, err
		}
		if reply.OK {
			fmt.Fprintf(os.Stderr, "配对成功，节点 %s\n", reply.Host)
			return reply, nil
		}
		if reply.Locked {
			return nil, errors.New(reply.Error)
		}
		fmt.Fprintf(os.Stderr, "第 %d 次失败: %s\n", attempt, reply.Error)
	}
	return nil, errors.New("配对失败次数过多，请重启 PGAcli 后重试")
}

func postPair(addr, code string) (*pairReply, error) {
	host, _ := os.Hostname()
	body, _ := json.Marshal(map[string]string{"code": code, "client": "PGAcli@" + host})

	resp, err := http.Post(normalizeAddr(addr)+"/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return nil, err
	}

	var reply pairReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("服务端返回了非预期内容（HTTP %d）", resp.StatusCode)
	}
	return &reply, nil
}

// selectPeer 在发现多个节点时让用户选择；只有一个则直接用。
func selectPeer(peers []discovery.Peer) discovery.Peer {
	if len(peers) == 1 {
		fmt.Fprintf(os.Stderr, "发现节点 %s (%s)\n", peers[0].Host, peers[0].IP)
		return peers[0]
	}

	fmt.Fprintln(os.Stderr, "\n发现多个节点：")
	for i, p := range peers {
		pairState := "可配对"
		if !p.Pairing {
			pairState = "需 Token"
		}
		fmt.Fprintf(os.Stderr, "  %d) %-16s %-15s v%s  %s\n", i+1, p.Host, p.IP, p.Version, pairState)
	}

	for {
		line, err := promptLine(fmt.Sprintf("请选择 [1-%d]: ", len(peers)))
		if err != nil {
			return peers[0]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return peers[0]
		}
		n := atoi(line)
		if n >= 1 && n <= len(peers) {
			return peers[n-1]
		}
		fmt.Fprintln(os.Stderr, "输入无效，请重新选择")
	}
}

// checkHealth 用短超时探活，避免连接失效节点时卡住。
func checkHealth(ctx context.Context, c *Client) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := c.Health(ctx)
	return err
}

func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// ---------- 节点管理命令 ----------

// cmdDiscover 只做扫描，不配对、不修改任何状态。
func cmdDiscover() error {
	port := *flagDiscPort
	fmt.Fprintf(os.Stderr, "正在扫描局域网 (UDP %d)...\n\n", port)

	peers, err := discovery.Probe(port, 1500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("广播失败: %w", err)
	}
	if len(peers) == 0 {
		fmt.Println("没有发现任何节点。排查顺序：")
		fmt.Println("  1. 被控端 上的 PGCact.exe 是否正在运行")
		fmt.Println("  2. 两台机器是否连在同一个路由器/同一网段（广播不跨网段）")
		fmt.Println("  3. 被控端 的防火墙是否放行了 UDP 7778")
		fmt.Println("  4. 若已知地址，可用 -addr 直接连接")
		return nil
	}

	store := loadCreds()
	rows := make([][]string, 0, len(peers))
	for _, p := range peers {
		state := "可配对"
		if !p.Pairing {
			state = "需 Token"
		}
		if _, ok := store.Get(p.NodeID); ok {
			state = "已配对"
			if store.DefaultNodeID() == p.NodeID {
				state = "已配对(默认)"
			}
		}
		rows = append(rows, []string{p.Host, p.IP, strconv.Itoa(p.HTTPPort), p.Version, state})
	}
	printTable([]string{"主机名", "IP", "端口", "版本", "状态"}, rows)
	fmt.Fprintf(os.Stderr, "\n共 %d 个节点。执行 PGAcli pair 接入\n", len(peers))
	return nil
}

// cmdPair 主动配对：优先用参数里的配对码，否则交互输入。
func cmdPair(args []string) error {
	store := loadCreds()

	// 允许 PGAcli pair 118608 这种把配对码直接写在参数里的用法
	_, rest := splitFlags(args)
	presetCode := ""
	if len(rest) > 0 && isAllDigits(rest[0]) {
		presetCode = rest[0]
	}

	addr := normalizeAddr(*flagAddr)
	var peer discovery.Peer

	if addr == "" {
		fmt.Fprintln(os.Stderr, "正在搜索局域网内的节点...")
		peers, err := discovery.Probe(*flagDiscPort, 1500*time.Millisecond)
		if err != nil {
			return fmt.Errorf("广播失败: %w", err)
		}
		if len(peers) == 0 {
			return errors.New("没有发现任何节点，请确认被控端 上的 PGCact 正在运行")
		}
		peer = selectPeer(peers)
		addr = peer.Addr()
	}

	// 该节点此前配对过且凭据仍有效，直接切换默认节点
	if peer.NodeID != "" {
		if c, ok := store.Get(peer.NodeID); ok {
			if err := checkHealth(context.Background(), NewClient(addr, c.Token)); err == nil {
				store.SetDefault(c.NodeID)
				store.Touch(c.NodeID, addr)
				if err := store.Save(); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "%s 已配对过，已切换为默认节点\n", c.Host)
				return nil
			}
		}
	}

	reply, err := pairWith(addr, peer.Host, presetCode)
	if err != nil {
		return err
	}

	nodeID := reply.NodeID
	if nodeID == "" {
		nodeID = peer.NodeID
	}
	store.Put(credential{
		NodeID:   nodeID,
		Host:     reply.Host,
		Addr:     addr,
		Token:    reply.Token,
		PairedAt: time.Now(),
		LastSeen: time.Now(),
	})
	store.SetDefault(nodeID)
	if err := store.Save(); err != nil {
		return fmt.Errorf("凭据保存失败: %w", err)
	}

	fmt.Printf("配对完成：%s (%s)\n", reply.Host, addr)
	fmt.Printf("凭据已保存到 %s\n", store.Path())
	fmt.Println("现在可以直接执行 PGAcli info")
	return nil
}

// cmdNodes 列出本地已保存的凭据。
func cmdNodes() error {
	store := loadCreds()
	nodes := store.List()
	if len(nodes) == 0 {
		fmt.Println("还没有配对任何节点。执行 PGAcli pair 开始配对")
		return nil
	}

	rows := make([][]string, 0, len(nodes))
	for _, n := range nodes {
		mark := ""
		if n.NodeID == store.DefaultNodeID() {
			mark = "*"
		}
		rows = append(rows, []string{
			mark, n.Host, n.Addr, n.NodeID,
			n.LastSeen.Format("2006-01-02 15:04"), n.PairedAt.Format("2006-01-02"),
		})
	}
	printTable([]string{"", "主机名", "地址", "节点ID", "最近使用", "配对时间"}, rows)
	fmt.Fprintf(os.Stderr, "\n* 表示默认节点。凭据文件: %s\n", store.Path())
	return nil
}

// cmdForget 删除某个节点的凭据。
func cmdForget(args []string) error {
	_, rest := splitFlags(args)
	if len(rest) < 1 {
		return errors.New("用法: PGAcli forget <主机名|节点ID>")
	}
	key := rest[0]

	store := loadCreds()
	found := ""
	for _, c := range store.List() {
		if c.NodeID == key || strings.EqualFold(c.Host, key) {
			found = c.NodeID
			break
		}
	}
	if found == "" {
		return fmt.Errorf("没有找到节点 %q，可用 PGAcli nodes 查看", key)
	}
	target, _ := store.Get(found)
	host := target.Host
	store.Remove(found)
	if err := store.Save(); err != nil {
		return err
	}
	fmt.Printf("已删除节点 %s 的凭据\n", host)
	return nil
}

// isAllDigits 判断字符串是否全部由数字组成，用于识别"把配对码当参数传"的用法。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
