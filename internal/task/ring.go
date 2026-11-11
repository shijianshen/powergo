package task

import "sync"

// RingBuffer 保留最近 max 字节的输出，并记录累计写入量，
// 让客户端可以通过 offset 增量拉取，而不必每次取全量。
type RingBuffer struct {
	mu    sync.Mutex
	data  []byte
	max   int
	total int64
}

// NewRingBuffer 创建环形缓冲区。
func NewRingBuffer(max int) *RingBuffer {
	if max <= 0 {
		max = 1 << 20
	}
	return &RingBuffer{max: max}
}

// WriteString 追加输出；超出上限时丢弃头部。
func (b *RingBuffer) WriteString(s string) {
	if s == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, s...)
	b.total += int64(len(s))

	if len(b.data) > b.max {
		cut := len(b.data) - b.max
		// 前移到 UTF-8 字符边界，避免缓冲区从一个汉字的半个字节开始
		for cut < len(b.data) && b.data[cut]&0xC0 == 0x80 {
			cut++
		}
		b.data = append(b.data[:0], b.data[cut:]...)
	}
}

// Size 返回当前缓冲区字节数。
func (b *RingBuffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.data))
}

// ReadFrom 返回 offset 之后的全部内容，next 为下次应传的 offset。
// 若 offset 早于缓冲区起点（内容已被截断），truncated 为 true。
func (b *RingBuffer) ReadFrom(offset int64) (data string, next int64, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	start := b.total - int64(len(b.data))
	if offset < start {
		offset = start
		truncated = true
	}
	if offset > b.total {
		offset = b.total
	}
	idx := offset - start
	if idx < 0 || idx > int64(len(b.data)) {
		return "", b.total, truncated
	}
	return string(b.data[idx:]), b.total, truncated
}
