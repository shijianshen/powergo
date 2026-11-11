package main

import "powergo/internal/remote"

// 凭据的读写与自愈逻辑统一由 internal/remote 提供，PGActl 只做别名与转发，
// 避免和 mcpd 出现两份实现。
type credential = remote.Credential
type credStore = remote.Store

func loadCreds() *credStore { return remote.LoadStore() }
