package rpc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
)

// Stream 负责把长任务的中间输出推回客户端。
//
// 两种模式：
//   - 流式（请求带 ?stream=1）：每条输出作为一个 notification 立刻 flush，客户端逐行读 NDJSON。
//   - 非流式：输出累积在缓冲区，任务结束后整体放进 result，避免 Agent 侧处理多条消息。
type Stream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	enabled bool
	buf     bytes.Buffer
}

// NewStream 包装 http.ResponseWriter。只有支持 Flusher 且显式要求流式时才真正流式输出。
func NewStream(w http.ResponseWriter, wantStream bool) *Stream {
	f, ok := w.(http.Flusher)
	return &Stream{
		w:       w,
		flusher: f,
		enabled: wantStream && ok,
	}
}

// Enabled 表示当前是否处于流式推送模式。
func (s *Stream) Enabled() bool {
	return s.enabled
}

// Emit 推送一段输出。kind 取值：stdout / stderr / progress。
func (s *Stream) Emit(kind, data string) {
	if data == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled {
		s.buf.WriteString(data)
		return
	}

	msg := Notification{
		JSONRPC: Version,
		Method:  "task.output",
		Params: map[string]interface{}{
			"stream": kind,
			"chunk":  data,
		},
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return
	}
	line = append(line, '\n')
	_, _ = s.w.Write(line)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Collected 返回非流式模式下累积的输出。
func (s *Stream) Collected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
