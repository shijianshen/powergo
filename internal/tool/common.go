package tool

import (
	"errors"
	"fmt"
	"os"

	"github.com/shijianshen/powergo/internal/rpc"
	"github.com/shijianshen/powergo/internal/sandbox"
)

// resolvePath 统一入口：先过沙箱，再把错误翻译成 Agent 能理解的 JSON-RPC 错误。
func resolvePath(c *Context, p string) (string, *rpc.Error) {
	abs, err := c.Box.Resolve(p)
	if err == nil {
		return abs, nil
	}
	if errors.Is(err, sandbox.ErrOutsideRoots) {
		return "", rpc.NewError(rpc.CodeSandboxDenied, "%v", err).
			WithData(map[string]interface{}{"allowed_roots": c.Box.Roots()})
	}
	return "", rpc.ErrInvalidParams("%v", err)
}

// fsErr 把 os 层错误映射为标准错误码。
func fsErr(err error, op, path string) *rpc.Error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return rpc.NewError(rpc.CodeNotFound, "%s 目标不存在: %s", op, path)
	case errors.Is(err, os.ErrPermission):
		return rpc.NewError(rpc.CodeSandboxDenied, "%s 权限不足: %s", op, path)
	case errors.Is(err, os.ErrExist):
		return rpc.NewError(rpc.CodeConflict, "%s 目标已存在: %s", op, path)
	default:
		return rpc.NewError(rpc.CodeInternalError, "%s 失败: %v", op, err)
	}
}

// clip 按字节上限截断输出，防止一次返回把 Agent 上下文冲爆。
func clip(s string, limit int64) (string, bool) {
	if limit <= 0 || int64(len(s)) <= limit {
		return s, false
	}
	cut := int(limit)
	// 回退到 UTF-8 字符边界
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut], true
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
