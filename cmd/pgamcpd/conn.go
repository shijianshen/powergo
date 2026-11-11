package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"powergo/internal/discovery"
	"powergo/internal/remote"
	"powergo/internal/rpc"
)

// connect 建立到 PGCact 的连接并缓存工具清单。调用方需持有锁时用 connectLocked。
func (s *serverState) connect(addrFlag, tokenFlag string, discPort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectLocked(addrFlag, tokenFlag, discPort)
}

func (s *serverState) connectLocked(addrFlag, tokenFlag string, discPort int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, cred, err := resolveClient(addrFlag, tokenFlag)
	if err != nil {
		return err
	}

	tools, err := client.Tools(ctx)
	if err != nil && cred.NodeID != "" {
		// 地址可能已经变了（DHCP 重新分配），广播重找一次
		if peers, derr := discovery.Probe(discPort, 900*time.Millisecond); derr == nil {
			for _, p := range peers {
				if p.NodeID != cred.NodeID {
					continue
				}
				candidate := remote.NewClient(p.Addr(), cred.Token)
				if t2, e2 := candidate.Tools(ctx); e2 == nil {
					client, tools, err = candidate, t2, nil
					store := remote.LoadStore()
					store.Touch(cred.NodeID, p.Addr())
					_ = store.Save()
					fmt.Fprintf(os.Stderr, "[PowerGo] 节点地址已更新为 %s\n", p.Addr())
				}
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("无法连接 %s: %w", client.Addr(), err)
	}

	s.client = client
	s.tools = tools
	s.host = cred.Host
	if s.host == "" {
		s.host = client.Addr()
	}

	s.mcpName = make(map[string]string, len(tools))
	s.agentMethod = make(map[string]string, len(tools))
	for _, t := range tools {
		name := sanitizeToolName(t.Name)
		s.mcpName[t.Name] = name
		s.agentMethod[name] = t.Name
	}
	return nil
}

// resolveClient 优先用命令行参数，其次用本地配对凭据。
func resolveClient(addrFlag, tokenFlag string) (*remote.Client, remote.Credential, error) {
	if addrFlag != "" {
		return remote.NewClient(addrFlag, tokenFlag), remote.Credential{Host: addrFlag}, nil
	}

	store := remote.LoadStore()
	c, ok := store.Current()
	if !ok {
		return nil, remote.Credential{}, errors.New(
			"本地没有配对凭据。请先在控制端 上执行 PGAcli pair 完成一次配对")
	}
	return remote.NewClient(c.Addr, c.Token), c, nil
}

// sanitizeToolName 把 fs.read 转成 fs_read。
// MCP 客户端普遍要求工具名匹配 ^[a-zA-Z0-9_-]{1,64}$，点号不在允许范围。
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "tool"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// renderResult 把 PGCact 的结果转成给 AI 看的文本。
// 文件类工具直接把正文吐出来，避免正文被包在 JSON 字符串里、换行全变成 \n。
func renderResult(raw json.RawMessage) string {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return string(raw)
	}

	if content, ok := obj["content"].(string); ok {
		delete(obj, "content")
		meta, _ := json.Marshal(obj)
		if len(obj) == 0 || string(meta) == "{}" {
			if content == "" {
				return "[空内容]"
			}
			return content
		}
		if content == "" {
			return "[空内容] 元信息: " + string(meta)
		}
		return content + "\n\n[元信息] " + string(meta)
	}

	return prettyJSON(raw)
}

func prettyJSON(raw json.RawMessage) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func formatRPCError(e *rpc.Error) string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf("[%d] %s", e.Code, e.Message)
	if e.Data != nil {
		if b, err := json.MarshalIndent(e.Data, "", "  "); err == nil {
			msg += "\n" + string(b)
		}
	}
	return msg
}
