package container

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// fixedClock returns an injectable clock for deterministic timestamp asserts.
func fixedClock() func() time.Time {
	t := time.Date(2026, 5, 22, 1, 30, 15, 123456789, time.UTC)
	return func() time.Time { return t }
}

func TestCRILogger_SingleShortLine(t *testing.T) {
	var buf bytes.Buffer
	l := newCRILogger(&buf)
	l.nowFn = fixedClock()
	if err := l.writeChunked(criStreamStdout, []byte("hello world")); err != nil {
		t.Fatalf("writeChunked: %v", err)
	}
	got := buf.String()
	want := "2026-05-22T01:30:15.123456789Z stdout F hello world\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestCRILogger_LongLineSplit(t *testing.T) {
	var buf bytes.Buffer
	l := newCRILogger(&buf)
	l.nowFn = fixedClock()

	// craft a line of 2.5 * maxLineBytes so we get P, P, F
	long := bytes.Repeat([]byte("x"), int(float64(maxLineBytes)*2.5))
	if err := l.writeChunked(criStreamStderr, long); err != nil {
		t.Fatalf("writeChunked: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 records (P,P,F), got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "stderr P ") {
		t.Errorf("line 0 not P: %q", lines[0])
	}
	if !strings.Contains(lines[1], "stderr P ") {
		t.Errorf("line 1 not P: %q", lines[1])
	}
	if !strings.Contains(lines[2], "stderr F ") {
		t.Errorf("line 2 not F: %q", lines[2])
	}
}

func TestPumpStream_MultipleLines(t *testing.T) {
	var buf bytes.Buffer
	l := newCRILogger(&buf)
	l.nowFn = fixedClock()

	in := strings.NewReader(".:53\nready\nlast line without newline")
	l.pumpStream(in, criStreamStdout)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), buf.String())
	}
	for i, suffix := range []string{".:53", "ready", "last line without newline"} {
		if !strings.HasSuffix(lines[i], "F "+suffix) {
			t.Errorf("line %d: %q does not end with %q", i, lines[i], "F "+suffix)
		}
	}
}

func TestPumpStream_StreamingPipe(t *testing.T) {
	// Drive pumpStream through an actual io.Pipe like real container stdio.
	var buf bytes.Buffer
	l := newCRILogger(&buf)
	l.nowFn = fixedClock()

	r, w := io.Pipe()
	done := make(chan struct{})
	go func() {
		l.pumpStream(r, criStreamStdout)
		close(done)
	}()

	_, _ = w.Write([]byte("hello\n"))
	_, _ = w.Write([]byte("world\n"))
	_ = w.Close()
	<-done

	got := buf.String()
	wantLines := []string{
		"2026-05-22T01:30:15.123456789Z stdout F hello",
		"2026-05-22T01:30:15.123456789Z stdout F world",
	}
	for _, l := range wantLines {
		if !strings.Contains(got, l) {
			t.Errorf("missing %q in:\n%s", l, got)
		}
	}
}

func TestCRILogger_StdoutAndStderrDontInterleave(t *testing.T) {
	// Each record line is atomic under the mutex—even if two goroutines write
	// concurrently, no record splice in the middle.
	var buf bytes.Buffer
	l := newCRILogger(&buf)
	l.nowFn = fixedClock()

	rOut, wOut := io.Pipe()
	rErr, wErr := io.Pipe()
	done := make(chan struct{}, 2)
	go func() { l.pumpStream(rOut, criStreamStdout); done <- struct{}{} }()
	go func() { l.pumpStream(rErr, criStreamStderr); done <- struct{}{} }()

	for i := 0; i < 50; i++ {
		_, _ = wOut.Write([]byte("aaaaaa\n"))
		_, _ = wErr.Write([]byte("bbbbbb\n"))
	}
	_ = wOut.Close()
	_ = wErr.Close()
	<-done
	<-done

	// Every line must be a complete CRI record: <ts> <stream> F <content>
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 4)
		if len(fields) != 4 {
			t.Errorf("malformed record: %q", line)
			continue
		}
		if fields[1] != "stdout" && fields[1] != "stderr" {
			t.Errorf("bad stream %q in: %q", fields[1], line)
		}
		if fields[2] != "F" && fields[2] != "P" {
			t.Errorf("bad tag %q in: %q", fields[2], line)
		}
	}
}
