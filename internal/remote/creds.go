package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Credential 是一台已配对节点的长期凭据。
//
// 关键设计：按 NodeID 索引而不是 IP。NodeID 只在重装系统时变化，
// 所以被控端 换了 IP 也能认回来，只需要更新 Addr。
type Credential struct {
	NodeID   string    `json:"node_id"`
	Host     string    `json:"host"`
	Addr     string    `json:"addr"`
	Token    string    `json:"token"`
	PairedAt time.Time `json:"paired_at"`
	LastSeen time.Time `json:"last_seen"`
}

// Store 是客户端凭据文件，存放在用户目录下，与工作目录无关。
type Store struct {
	Default string                `json:"default"`
	Nodes   map[string]Credential `json:"nodes"`
	path    string
}

// credFilePath 返回凭据文件路径，可用环境变量 POWERGO_HOME 覆盖（便于测试）。
func CredFilePath() string {
	if dir := os.Getenv("POWERGO_HOME"); dir != "" {
		return filepath.Join(dir, "credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}

	primary := filepath.Join(home, ".powergo", "credentials.json")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	// 兼容 0.x 的目录名，避免升级后需要重新配对
	legacy := filepath.Join(home, ".juyuagent", "credentials.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return primary
}

func LoadStore() *Store {
	s := &Store{Nodes: map[string]Credential{}, path: CredFilePath()}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s)
	if s.Nodes == nil {
		s.Nodes = map[string]Credential{}
	}
	if s.path == "" {
		s.path = CredFilePath()
	}
	return s
}

func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建凭据目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	// 先写临时文件再改名，避免写到一半断电把凭据弄坏
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) get(nodeID string) (Credential, bool) {
	c, ok := s.Nodes[nodeID]
	return c, ok
}

func (s *Store) put(c Credential) {
	if s.Nodes == nil {
		s.Nodes = map[string]Credential{}
	}
	s.Nodes[c.NodeID] = c
	if s.Default == "" {
		s.Default = c.NodeID
	}
}

// current 返回默认节点凭据。
func (s *Store) current() (Credential, bool) {
	if s.Default != "" {
		if c, ok := s.Nodes[s.Default]; ok {
			return c, true
		}
	}
	// 默认节点缺失时退回第一个可用节点
	for _, c := range s.Nodes {
		return c, true
	}
	return Credential{}, false
}

// findByAddr 按地址反查凭据，用于用户显式传 -addr 时复用已有 Token。
func (s *Store) findByAddr(addr string) (Credential, bool) {
	normalized := NormalizeAddr(addr)
	for _, c := range s.Nodes {
		if NormalizeAddr(c.Addr) == normalized {
			return c, true
		}
	}
	return Credential{}, false
}

// touch 更新节点的地址与最近使用时间，实现 IP 变化后的自愈。
func (s *Store) touch(nodeID, addr string) {
	c, ok := s.Nodes[nodeID]
	if !ok {
		return
	}
	if addr != "" {
		c.Addr = addr
	}
	c.LastSeen = time.Now()
	s.Nodes[nodeID] = c
}

// list 返回按主机名排序的节点列表。
func (s *Store) list() []Credential {
	out := make([]Credential, 0, len(s.Nodes))
	for _, c := range s.Nodes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func (s *Store) remove(nodeID string) bool {
	if _, ok := s.Nodes[nodeID]; !ok {
		return false
	}
	delete(s.Nodes, nodeID)
	if s.Default == nodeID {
		s.Default = ""
		for id := range s.Nodes {
			s.Default = id
			break
		}
	}
	return true
}

var ErrNoCredentials = errors.New("尚未配对任何节点")

// Path 返回凭据文件路径。
func (s *Store) Path() string { return s.path }

// DefaultNodeID 返回默认节点 ID。
func (s *Store) DefaultNodeID() string { return s.Default }

// SetDefault 设置默认节点。
func (s *Store) SetDefault(nodeID string) { s.Default = nodeID }

// Put 保存一条凭据。
func (s *Store) Put(c Credential) { s.put(c) }

// Get 读取一条凭据。
func (s *Store) Get(nodeID string) (Credential, bool) { return s.get(nodeID) }

// Current 返回默认节点。
func (s *Store) Current() (Credential, bool) { return s.current() }

// FindByAddr 按地址反查。
func (s *Store) FindByAddr(addr string) (Credential, bool) { return s.findByAddr(addr) }

// Touch 更新节点地址与最近使用时间。
func (s *Store) Touch(nodeID, addr string) { s.touch(nodeID, addr) }

// List 返回全部节点。
func (s *Store) List() []Credential { return s.list() }

// Remove 删除节点。
func (s *Store) Remove(nodeID string) bool { return s.remove(nodeID) }

// Save 持久化。
func (s *Store) Save() error { return s.save() }
