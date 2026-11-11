// Package winenc 处理 Windows 中文控制台的编码问题。
//
// 中文版 Windows 7/10 的 cmd.exe 输出默认是 GBK（代码页 936），
// 而 Go 内部与 Agent 侧全部按 UTF-8 处理，不做转换会得到满屏乱码。
package winenc

import (
	"bytes"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// Decode 自动识别并转为 UTF-8：优先按 UTF-8 解释，非法时按 GB18030 解码。
// 用 GB18030 而非 GBK，因为它是 GBK 的超集，兼容面更宽。
func Decode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), b)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// EncodeGBK 把 UTF-8 文本编码为 GBK，用于向只认本地代码页的子进程写 stdin。
func EncodeGBK(s string) ([]byte, error) {
	return simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(s))
}

// DefaultLineLimit 是单行缓冲的强制切分阈值（64KB）。
const DefaultLineLimit = 64 << 10

// LineDecoder 把字节流切成"完整行"再逐行转码。
//
// 不能对任意字节块单独调用 Decode —— 一个 GBK 汉字占两字节，
// 分块边界可能正好切在中间，逐块解码会产出乱码。按行切分是安全的：
// GBK 双字节字符的第二节节不会等于 0x0A。
type LineDecoder struct {
	buf   []byte
	Limit int
}

// Feed 喂入新数据，返回已经完整的行（已转 UTF-8，不含行尾换行符）。
func (d *LineDecoder) Feed(p []byte) []string {
	if d.Limit <= 0 {
		d.Limit = DefaultLineLimit
	}
	d.buf = append(d.buf, p...)

	var lines []string
	for {
		idx := bytes.IndexByte(d.buf, '\n')
		if idx < 0 {
			break
		}
		line := d.buf[:idx]
		d.buf = d.buf[idx+1:]
		lines = append(lines, decodeLine(line))
	}

	// 长时间不换行的输出（进度条、日志）不能无限堆积，达到阈值就强制吐出。
	for d.Limit > 0 && len(d.buf) >= d.Limit {
		cut := safeBoundary(d.buf, d.Limit)
		if cut <= 0 {
			break
		}
		lines = append(lines, Decode(d.buf[:cut]))
		d.buf = d.buf[cut:]
	}
	return lines
}

// Flush 在进程退出后取走剩余未换行的内容。
func (d *LineDecoder) Flush() []string {
	if len(d.buf) == 0 {
		return nil
	}
	rest := Decode(d.buf)
	d.buf = d.buf[:0]
	return []string{rest}
}

func decodeLine(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return Decode(b)
}

// safeBoundary 在 limit 之前找一个 ASCII 字节位置作为切分点。
// GBK 的尾字节和 UTF-8 的续字节都 >= 0x80，所以 ASCII 字节一定落在字符边界上。
func safeBoundary(b []byte, limit int) int {
	if limit > len(b) {
		limit = len(b)
	}
	for i := limit; i > 0; i-- {
		if b[i-1] < 0x80 {
			return i
		}
	}
	return 0
}
