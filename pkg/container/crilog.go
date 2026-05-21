package container

import (
	"bufio"
	"fmt"
	"io"
	"sync"
	"time"
)

// CRI log format (from kubernetes/cri-api):
//
//	<RFC3339Nano timestamp> <stream> <P|F> <log line>\n
//
// where:
//   - stream  = "stdout" | "stderr"
//   - tag     = "P" (partial: original line was longer than maxLineBytes
//     and got split) | "F" (full: this is the end of a logical line)
//
// Example:
//
//	2026-05-22T01:30:15.123456789Z stdout F .:53
//
// Kubelet's logger (ParseLogLine) requires this exact format. If the file
// contains plain content like ".:53\n", `kubectl logs` shows
// "failed to get parse function: unsupported log format".
const (
	criStreamStdout = "stdout"
	criStreamStderr = "stderr"

	// 16 KiB — same threshold containerd / cri-o use. Lines longer than this
	// will be split into multiple P records followed by one F record.
	maxLineBytes = 16 * 1024
)

// criLogger wraps an underlying io.Writer (typically the per-container log
// file) and serializes every Write/line into CRI log format. It is safe for
// concurrent use: stdout & stderr goroutines share one logger so their lines
// don't interleave mid-line.
type criLogger struct {
	mu sync.Mutex
	w  io.Writer
	// nowFn allows tests to inject a deterministic clock.
	nowFn func() time.Time
}

func newCRILogger(w io.Writer) *criLogger {
	return &criLogger{w: w, nowFn: time.Now}
}

// emitLine writes one logical line (already <= maxLineBytes per chunk) under
// the lock so stdout/stderr lines stay atomic.
func (l *criLogger) emitLine(stream, tag, line string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.nowFn().UTC().Format(time.RFC3339Nano)
	_, err := fmt.Fprintf(l.w, "%s %s %s %s\n", ts, stream, tag, line)
	return err
}

// pumpStream reads from r line by line and writes each line into the CRI log.
// Long lines (>maxLineBytes) are split: a sequence of P records followed by
// one F record at the end. EOF cleanly drains the goroutine.
//
// The function blocks until r returns EOF or a non-temporary error; on error
// it logs to stderr and returns. It is intended to be invoked as `go pumpStream(...)`.
func (l *criLogger) pumpStream(r io.Reader, stream string) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if writeErr := l.writeChunked(stream, line); writeErr != nil {
				// Write error to log file is best-effort; keep draining the pipe.
				_ = writeErr
			}
		}
		if err != nil {
			return
		}
	}
}

// readLine reads up to the next '\n'. If the buffer fills before \n is seen
// (line longer than bufio internal buffer), partial data is returned together
// with no error so the caller can keep reading; otherwise it returns
// the line and io.EOF on stream end.
//
// Returned []byte does NOT include the trailing '\n'.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		// chunk includes '\n' on success. ErrBufferFull means buffer was filled
		// without seeing '\n' — we have a partial line, keep reading.
		if len(chunk) > 0 {
			if err == bufio.ErrBufferFull {
				buf = append(buf, chunk...)
				continue
			}
			// strip trailing \n if present
			if chunk[len(chunk)-1] == '\n' {
				chunk = chunk[:len(chunk)-1]
			}
			if buf == nil {
				return chunk, err
			}
			buf = append(buf, chunk...)
			return buf, err
		}
		// chunk empty
		return buf, err
	}
}

// writeChunked breaks a logical line longer than maxLineBytes into a sequence
// of P records and a final F record. Lines <= maxLineBytes go out as a single F.
func (l *criLogger) writeChunked(stream string, line []byte) error {
	for len(line) > maxLineBytes {
		if err := l.emitLine(stream, "P", string(line[:maxLineBytes])); err != nil {
			return err
		}
		line = line[maxLineBytes:]
	}
	return l.emitLine(stream, "F", string(line))
}
