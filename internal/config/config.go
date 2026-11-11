// Package config 负责配置加载、默认值与首次启动的密钥生成。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// discoveryPortDefault 与 discovery.DefaultPort 保持一致，这里避免循环依赖。
const discoveryPortDefault = 7778

// Config 是 agentd 的运行配置，对应 agentd.json。
type Config struct {
	Listen            string   `json:"listen"`
	Token             string   `json:"token"`
	AllowedRoots      []string `json:"allowed_roots"`
	AllowIP           []string `json:"allow_ip"`
	DefaultTimeoutSec int      `json:"default_timeout_sec"`
	MaxTimeoutSec     int      `json:"max_timeout_sec"`
	MaxReadBytes      int64    `json:"max_read_bytes"`
	MaxOutputBytes    int64    `json:"max_output_bytes"`
	PythonPath        string   `json:"python_path"`
	WorkDir           string   `json:"work_dir"`
	LogFile           string   `json:"log_file"`

	// NodeID 是节点稳定标识，重装系统才会变。客户端凭据按它匹配，
	// 因此 IP 变化后仍能认回同一台机器。
	NodeID string `json:"node_id"`

	// PairCode 是 6 位配对码，显示在控制台，用于首次免 Token 接入。
	// 配对成功后会轮换，避免被重复使用。
	PairCode string `json:"pair_code"`

	// PairingEnabled 控制是否开放配对；用指针是为了区分"未配置"和"显式关闭"。
	PairingEnabled *bool `json:"pairing_enabled"`

	// DiscoveryPort 是 UDP 发现端口；显式写 0 表示关闭广播发现。
	DiscoveryPort *int `json:"discovery_port"`

	// GeneratedToken 标记本次启动是否新生成了凭据，供 banner 提示用。
	GeneratedToken bool `json:"-"`
	// RotatedPairs 记录配对成功后是否轮换了配对码，需要回写配置文件。
	RotatedPairs bool `json:"-"`
}

// Default 返回一份可直接运行的配置。
func Default() *Config {
	wd, _ := os.Getwd()
	on := true
	discPort := discoveryPortDefault
	return &Config{
		Listen:            "0.0.0.0:7777",
		AllowedRoots:      detectRoots(),
		AllowIP:           []string{},
		DefaultTimeoutSec: 60,
		MaxTimeoutSec:     1800,
		MaxReadBytes:      4 << 20,
		MaxOutputBytes:    8 << 20,
		PythonPath:        "",
		WorkDir:           wd,
		LogFile:           "agentd.log",
		PairingEnabled:    &on,
		DiscoveryPort:     &discPort,
	}
}

// PairingOn 返回是否开放配对。
func (c *Config) PairingOn() bool {
	return c.PairingEnabled == nil || *c.PairingEnabled
}

// DiscoveryPortOr 返回实际使用的 UDP 发现端口，0 表示不启用发现。
func (c *Config) DiscoveryPortOr() int {
	if c.DiscoveryPort == nil {
		return discoveryPortDefault
	}
	return *c.DiscoveryPort
}

// Load 读取配置文件；文件不存在时用默认值创建并写入（含自动生成的 Token）。
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("读取配置失败: %w", err)
		}
		if _, err := cfg.ensureSecrets(); err != nil {
			return nil, err
		}
		if err := cfg.Save(path); err != nil {
			return nil, fmt.Errorf("写入初始配置失败: %w", err)
		}
		return cfg, nil
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	cfg.applyDefaults()

	changed, err := cfg.ensureSecrets()
	if err != nil {
		return nil, err
	}
	if changed {
		if err := cfg.Save(path); err != nil {
			return nil, fmt.Errorf("回写凭据失败: %w", err)
		}
	}
	return cfg, nil
}

// Save 以 UTF-8 缩进格式写回配置文件。
func (c *Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Validate 在启动前做基本校验，尽早暴露配置错误。
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen 不能为空")
	}
	if len(c.AllowedRoots) == 0 {
		return errors.New("allowed_roots 不能为空，否则所有文件操作都会被拒绝")
	}
	if c.DefaultTimeoutSec <= 0 {
		c.DefaultTimeoutSec = 60
	}
	if c.MaxTimeoutSec < c.DefaultTimeoutSec {
		c.MaxTimeoutSec = c.DefaultTimeoutSec
	}
	if c.WorkDir == "" {
		c.WorkDir, _ = os.Getwd()
	}
	for i, r := range c.AllowedRoots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return fmt.Errorf("allowed_roots[%d] 非法: %w", i, err)
		}
		c.AllowedRoots[i] = abs
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:7777"
	}
	if c.DefaultTimeoutSec <= 0 {
		c.DefaultTimeoutSec = 60
	}
	if c.MaxTimeoutSec <= 0 {
		c.MaxTimeoutSec = 1800
	}
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = 4 << 20
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8 << 20
	}
	if len(c.AllowedRoots) == 0 {
		c.AllowedRoots = detectRoots()
	}
	if c.LogFile == "" {
		c.LogFile = "agentd.log"
	}
	if c.WorkDir == "" {
		c.WorkDir, _ = os.Getwd()
	}
}

func (c *Config) fillToken() error {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("生成 Token 失败: %w", err)
	}
	c.Token = hex.EncodeToString(buf)
	c.GeneratedToken = true
	return nil
}

// ensureSecrets 补齐 Token、NodeID、PairCode 三项凭据，返回是否产生了新值。
func (c *Config) ensureSecrets() (bool, error) {
	changed := false

	if c.Token == "" {
		if err := c.fillToken(); err != nil {
			return false, err
		}
		changed = true
	}

	if c.NodeID == "" {
		id, err := randomHex(6)
		if err != nil {
			return false, err
		}
		c.NodeID = "node_" + id
		changed = true
	}

	if c.PairCode == "" && c.PairingOn() {
		code, err := randomCode(6)
		if err != nil {
			return false, err
		}
		c.PairCode = code
		changed = true
	}

	return changed, nil
}

// RotatePairCode 在配对成功后轮换配对码，旧码立即失效。
func (c *Config) RotatePairCode() (string, error) {
	code, err := randomCode(6)
	if err != nil {
		return "", err
	}
	c.PairCode = code
	c.RotatedPairs = true
	return code, nil
}

// randomHex 生成 n 字节的十六进制标识。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机标识失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// randomCode 生成 n 位纯数字配对码，便于从控制台抄写。
func randomCode(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成配对码失败: %w", err)
	}
	var b strings.Builder
	for _, v := range buf {
		b.WriteByte(byte('0' + v%10))
	}
	return b.String(), nil
}

// detectRoots 枚举当前存在的盘符，作为文件访问白名单的默认值。
func detectRoots() []string {
	var roots []string
	for ch := 'A'; ch <= 'Z'; ch++ {
		p := string(ch) + ":\\"
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	if len(roots) == 0 {
		wd, _ := os.Getwd()
		roots = []string{wd}
	}
	return roots
}
