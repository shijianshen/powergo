// Package sandbox 把 Agent 的文件访问约束在配置的根目录白名单内。
//
// 电脑 C 上是真实的生产环境，AI 误判一次路径就可能删到系统目录，
// 所以每个 fs 操作都必须先过这一层。
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrEmptyPath    = errors.New("路径不能为空")
	ErrOutsideRoots = errors.New("路径超出允许访问的根目录")
	ErrInvalidPath  = errors.New("路径非法")
)

// Sandbox 保存规范化后的小写根目录列表，便于 Windows 大小写不敏感比较。
type Sandbox struct {
	roots []string
}

// New 根据配置里的 allowed_roots 构造沙箱。
func New(roots []string) *Sandbox {
	s := &Sandbox{}
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		s.roots = append(s.roots, filepath.Clean(abs))
	}
	return s
}

// Roots 返回已生效的根目录，用于 /health 自检展示。
func (s *Sandbox) Roots() []string {
	out := make([]string, len(s.roots))
	copy(out, s.roots)
	return out
}

// Resolve 校验并返回绝对路径。空路径会被拒绝，越界路径返回 ErrOutsideRoots。
func (s *Sandbox) Resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(p, 0) {
		return "", ErrInvalidPath
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	abs = filepath.Clean(abs)

	// 符号链接 / 目录联接可能把路径指到白名单之外，存在时按真实路径再校验一次。
	checked := abs
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		checked = filepath.Clean(real)
	}

	for _, root := range s.roots {
		if within(checked, root) || within(abs, root) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrOutsideRoots, abs)
}

// MustResolve 供内部调用，错误只可能是越界或空路径。
func (s *Sandbox) MustResolve(p string) (string, error) {
	return s.Resolve(p)
}

// within 判断 p 是否等于 root 或位于 root 之下。
// 必须给 root 补上分隔符再比较，否则 C:\foo 会误判包含 C:\foobar。
func within(p, root string) bool {
	lp := strings.ToLower(p)
	lr := strings.ToLower(filepath.Clean(root))
	if lp == lr {
		return true
	}
	if !strings.HasSuffix(lr, string(os.PathSeparator)) {
		lr += string(os.PathSeparator)
	}
	return strings.HasPrefix(lp, lr)
}
