package main

import (
	"encoding/json"

	"juyuagent/internal/remote"
)

// Client 直接复用公共实现，commands.go 里的用法保持不变。
type Client = remote.Client

// NewClient 创建到 agentd 的连接。
func NewClient(addr, token string) *Client { return remote.NewClient(addr, token) }

func normalizeAddr(a string) string { return remote.NormalizeAddr(a) }

func unwrap(raw json.RawMessage, v interface{}) error { return remote.Unwrap(raw, v) }
