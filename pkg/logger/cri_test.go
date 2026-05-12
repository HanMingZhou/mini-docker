package logger

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCRIWriterFullLines(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	w := NewCRIWriter(sink, Stdout)

	w.Write([]byte("hello\nworld\n"))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		parts := strings.SplitN(line, " ", 4)
		if len(parts) != 4 {
			t.Fatalf("line %d: want 4 fields, got %d: %q", i, len(parts), line)
		}
		if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
			t.Errorf("line %d: bad timestamp %q: %v", i, parts[0], err)
		}
		if parts[1] != "stdout" {
			t.Errorf("line %d: stream = %q, want stdout", i, parts[1])
		}
		if parts[2] != "F" {
			t.Errorf("line %d: tag = %q, want F", i, parts[2])
		}
	}
	if !strings.Contains(lines[0], "hello") || !strings.Contains(lines[1], "world") {
		t.Errorf("payload missing: %q", buf.String())
	}
}

func TestCRIWriterPartialFlush(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	w := NewCRIWriter(sink, Stderr)

	// Write two partial chunks, then complete them with a \n
	w.Write([]byte("he"))
	if buf.Len() != 0 {
		t.Fatalf("buffer leaked before newline: %q", buf.String())
	}
	w.Write([]byte("llo\nworld"))
	// After this write: "hello\n" should be flushed as one F line,
	// "world" still buffered.
	if !strings.Contains(buf.String(), "stderr F hello") {
		t.Errorf("missing F-line: %q", buf.String())
	}
	if strings.Contains(buf.String(), "world") {
		t.Errorf("partial flushed too early: %q", buf.String())
	}

	// Close flushes the buffered partial line as P.
	w.Close()
	if !strings.Contains(buf.String(), "stderr P world") {
		t.Errorf("close should flush P-line: %q", buf.String())
	}
}

func TestCRIWriterInterleaved(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	stdout := NewCRIWriter(sink, Stdout)
	stderr := NewCRIWriter(sink, Stderr)

	stdout.Write([]byte("a\n"))
	stderr.Write([]byte("b\n"))
	stdout.Write([]byte("c\n"))

	out := buf.String()
	// Expect interleaved lines with correct stream labels.
	if !strings.Contains(out, "stdout F a") {
		t.Errorf("missing stdout a: %q", out)
	}
	if !strings.Contains(out, "stderr F b") {
		t.Errorf("missing stderr b: %q", out)
	}
	if !strings.Contains(out, "stdout F c") {
		t.Errorf("missing stdout c: %q", out)
	}
}

func TestCRIWriterLargePartial(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	w := NewCRIWriter(sink, Stdout)

	big := strings.Repeat("x", maxLineBytes+100)
	w.Write([]byte(big)) // no newline
	// Should have forced a P-tagged flush.
	if !strings.Contains(buf.String(), "stdout P ") {
		t.Errorf("expected forced P flush, got %q...", buf.String()[:min(200, buf.Len())])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
