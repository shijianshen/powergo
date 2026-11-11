// Package discovery 实现局域网自动发现。
//
// 电脑 C 的 IP 由 DHCP 分配，随时可能变化，靠人工维护地址不现实。
// 这里用 UDP 广播做服务发现：客户端朝 255.255.255.255 和各网卡的定向广播地址
// 发一个探测包，局域网内所有 agentd 单播回应自己的节点信息。
//
// 发现只负责"找到"，认证靠 pairing 流程拿到 Token，两者分离。
package discovery

import (
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// Magic 用于快速过滤无关的 UDP 包
	Magic = "JYAGENT"

	// Proto 是协议版本，未来加字段时客户端可据此兼容
	Proto = "1"

	// DefaultPort 是发现服务的默认端口
	DefaultPort = 7778

	KindProbe = "probe"
	KindReply = "reply"
)

// Packet 是探测与响应共用的报文结构。
type Packet struct {
	Magic        string `json:"magic"`
	Proto        string `json:"proto"`
	Kind         string `json:"kind"`
	Nonce        string `json:"nonce,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
	Host         string `json:"host,omitempty"`
	HTTPPort     int    `json:"http_port,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	Pairing      bool   `json:"pairing"`
}

// Peer 是发现到的可用节点。
type Peer struct {
	NodeID    string
	Host      string
	IP        string
	HTTPPort  int
	Version   string
	Pairing   bool
	LatencyMS int64
}

// Addr 返回可直接用于 HTTP 请求的地址。
func (p Peer) Addr() string {
	host := p.IP
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + ":" + strconv.Itoa(p.HTTPPort)
}

// Responder 根据探测包生成回应包。返回 nil 表示忽略。
type Responder func(probe *Packet, remoteIP string) *Packet

// Serve 在当前进程内监听发现端口，收到探测就回调 Responder 并单播回应。
// 该函数阻塞，应在独立 goroutine 中调用。
func Serve(port int, respond Responder) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		return err
	}
	defer conn.Close()

	buf := make([]byte, 2048)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			// 单个包读取失败不应终止整个监听
			continue
		}

		var probe Packet
		if json.Unmarshal(buf[:n], &probe) != nil {
			continue
		}
		if probe.Magic != Magic || probe.Kind != KindProbe {
			continue
		}

		reply := respond(&probe, remote.IP.String())
		if reply == nil {
			continue
		}
		reply.Magic = Magic
		reply.Proto = Proto
		reply.Kind = KindReply
		reply.Nonce = probe.Nonce

		data, err := json.Marshal(reply)
		if err != nil {
			continue
		}
		_, _ = conn.WriteToUDP(data, remote)
	}
}

// Probe 广播探测并收集回应。timeout 建议 600ms~1s，太短会漏掉慢设备。
func Probe(port int, timeout time.Duration) ([]Peer, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := enableBroadcast(conn); err != nil {
		return nil, err
	}

	probe := Packet{Magic: Magic, Proto: Proto, Kind: KindProbe, Nonce: randomNonce()}
	data, err := json.Marshal(probe)
	if err != nil {
		return nil, err
	}

	// 同时发往全网广播和各网卡的定向广播，后者在部分路由器上更可靠
	targets := broadcastTargets(port)
	for _, addr := range targets {
		_, _ = conn.WriteToUDP(data, addr)
	}

	seen := make(map[string]Peer)
	deadline := time.Now().Add(timeout)

	buf := make([]byte, 2048)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(remain))

		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			// 读超时即视为收集结束
			break
		}

		var reply Packet
		if json.Unmarshal(buf[:n], &reply) != nil {
			continue
		}
		if reply.Magic != Magic || reply.Kind != KindReply {
			continue
		}
		if reply.Nonce != probe.Nonce {
			continue
		}
		if reply.NodeID == "" {
			continue
		}

		if _, dup := seen[reply.NodeID]; dup {
			continue
		}
		seen[reply.NodeID] = Peer{
			NodeID:   reply.NodeID,
			Host:     reply.Host,
			IP:       remote.IP.String(),
			HTTPPort: reply.HTTPPort,
			Version:  reply.AgentVersion,
			Pairing:  reply.Pairing,
		}
	}

	peers := make([]Peer, 0, len(seen))
	for _, p := range seen {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Host < peers[j].Host })
	return peers, nil
}

// enableBroadcast 打开 SO_BROADCAST。
// net.UDPConn 没有直接暴露这个选项，需要通过 RawConn 拿到文件描述符设置。
func enableBroadcast(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if cerr := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); cerr != nil {
		return cerr
	}
	return sockErr
}

// broadcastTargets 组装广播地址列表。
func broadcastTargets(port int) []*net.UDPAddr {
	var out []*net.UDPAddr

	if addr, err := net.ResolveUDPAddr("udp4", "255.255.255.255:"+strconv.Itoa(port)); err == nil {
		out = append(out, addr)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			mask := net.IP(ipnet.Mask).To4()
			if mask == nil {
				continue
			}
			bcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bcast[i] = ip4[i] | ^mask[i]
			}
			if addr, err := net.ResolveUDPAddr("udp4", bcast.String()+":"+strconv.Itoa(port)); err == nil {
				out = append(out, addr)
			}
		}
	}
	return out
}

var nonceMu sync.Mutex
var nonceSeq uint32

// randomNonce 生成探测包序号，用于剔除过期的异步回应。
func randomNonce() string {
	nonceMu.Lock()
	defer nonceMu.Unlock()
	nonceSeq++
	return strconv.Itoa(int(time.Now().UnixNano()&0xFFFFFF)) + "-" + strconv.Itoa(int(nonceSeq))
}
