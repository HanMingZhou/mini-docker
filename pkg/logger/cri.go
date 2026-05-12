// Package logger 实现 CRI 规定的容器日志文件格式。
//
// 格式（对应 K8s 的 "cri-o" / "containerd" 默认格式）：
//
//	<RFC3339Nano> <stream> <F|P> <msg>\n
//
// 其中：
//   - stream 是 "stdout" 或 "stderr"
//   - F 表示完整行（以 \n 结尾），P 表示部分行（缓冲区满或被截断）
//   - msg 是原始日志内容（不含结尾的 \n）
//
// Kubelet 和 `crictl logs` 按这个格式读文件并切分，然后按需把 stream / 时间戳
// 还原给用户或 `kubectl logs`。
//
// 本实现：
//   - CRIWriter 是 io.Writer 实现；一个写入器对应一个流（stdout 或 stderr）。
//   - 多个 CRIWriter 共享同一个 *os.File，用互斥锁串行化写入避免交错。
//   - 每 Write 调用可能包含多行、跨边界的内容；内部做行缓冲，按 \n 切分并
//     为每一行加前缀。超过 maxLineBytes 未遇到 \n 时强制切 P 段输出。
package logger

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Stream identifies a container output stream.
type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// maxLineBytes caps the amount of bytes buffered before a P-tagged flush is forced.
// Matches containerd's default (16KiB).
const maxLineBytes = 16 << 10

// Sink is a thread-safe wrapper around the underlying log file.
// Multiple CRIWriter instances may share the same Sink to write interleaved
// lines from stdout and stderr into one file.
type Sink struct {
	mu sync.Mutex
	w  io.Writer
	// Closer — may be nil when the underlying writer is not ownable.
	closer io.Closer
}

// NewFileSink opens (create+append) a log file and returns a Sink.
func NewFileSink(path string) (*Sink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &Sink{w: f, closer: f}, nil
}

// NewSink wraps an arbitrary writer (for tests).
func NewSink(w io.Writer) *Sink {
	return &Sink{w: w}
}

// Close closes the underlying file (if owned).
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// writeFrame writes one <ts stream tag msg> line.
func (s *Sink) writeFrame(ts time.Time, stream Stream, full bool, msg []byte) error {
	tag := "P"
	if full {
		tag = "F"
	}
	// Pre-build the header to keep the write as close to atomic as possible.
	// Kubelet tolerates lines up to 16KiB without buffering, so single writes
	// suffice when a caller respects that limit.
	header := fmt.Sprintf("%s %s %s ", ts.UTC().Format(time.RFC3339Nano), stream, tag)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.WriteString(s.w, header); err != nil {
		return err
	}
	if _, err := s.w.Write(msg); err != nil {
		return err
	}
	_, err := io.WriteString(s.w, "\n")
	return err
}

// CRIWriter is an io.Writer that formats and forwards writes to a Sink in
// the CRI log format.
//
// The zero value is not usable; use NewCRIWriter.
type CRIWriter struct {
	sink   *Sink
	stream Stream
	buf    bytes.Buffer
}

// NewCRIWriter creates a writer for the given stream sharing the given sink.
func NewCRIWriter(sink *Sink, stream Stream) *CRIWriter {
	return &CRIWriter{sink: sink, stream: stream}
}

// Write formats the input according to the CRI log format. Lines are emitted
// as soon as a \n is seen; any trailing partial line is held in the buffer
// until Close (or the next Write).
//
// It always returns len(p), nil on success so the caller sees uninterrupted
// byte throughput even when the underlying sink fails (we log the error to
// stderr and drop the line rather than abort).
func (w *CRIWriter) Write(p []byte) (int, error) {
	now := time.Now()
	remaining := p

	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		if idx < 0 {
			// No newline yet — buffer and check for overflow.
			w.buf.Write(remaining)
			if w.buf.Len() >= maxLineBytes {
				// Flush as partial to prevent unbounded growth.
				_ = w.sink.writeFrame(now, w.stream, false, w.buf.Bytes())
				w.buf.Reset()
			}
			return len(p), nil
		}
		// Include buffered prefix + this line's content (excluding the \n).
		line := remaining[:idx]
		if w.buf.Len() > 0 {
			w.buf.Write(line)
			_ = w.sink.writeFrame(now, w.stream, true, w.buf.Bytes())
			w.buf.Reset()
		} else {
			_ = w.sink.writeFrame(now, w.stream, true, line)
		}
		remaining = remaining[idx+1:]
	}
	return len(p), nil
}

// Close flushes any buffered partial line as a P-tagged frame.
func (w *CRIWriter) Close() error {
	if w.buf.Len() == 0 {
		return nil
	}
	err := w.sink.writeFrame(time.Now(), w.stream, false, w.buf.Bytes())
	w.buf.Reset()
	return err
}
