package build

import (
	"strings"
	"testing"
)

func TestParseSimple(t *testing.T) {
	src := `
# Comment
FROM busybox:latest

RUN echo hello > /out
COPY config.json /etc/app/
ENV FOO=bar
WORKDIR /app
CMD ["/bin/sh"]
`
	insts, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	wantOps := []string{"FROM", "RUN", "COPY", "ENV", "WORKDIR", "CMD"}
	if len(insts) != len(wantOps) {
		t.Fatalf("got %d insts, want %d", len(insts), len(wantOps))
	}
	for i, want := range wantOps {
		if insts[i].Op != want {
			t.Errorf("inst[%d].Op = %q, want %q", i, insts[i].Op, want)
		}
	}
	if insts[0].Raw != "busybox:latest" {
		t.Errorf("FROM raw = %q", insts[0].Raw)
	}
	if insts[1].Raw != "echo hello > /out" {
		t.Errorf("RUN raw = %q", insts[1].Raw)
	}
}

func TestParseLineContinuation(t *testing.T) {
	src := `RUN apt-get update \
    && apt-get install -y \
    nginx curl`
	insts, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 {
		t.Fatalf("got %d insts, want 1", len(insts))
	}
	if !strings.Contains(insts[0].Raw, "apt-get update") ||
		!strings.Contains(insts[0].Raw, "nginx curl") {
		t.Errorf("continuation join failed: %q", insts[0].Raw)
	}
}

func TestParseJSONArray(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`["/bin/sh","-c","echo"]`, []string{"/bin/sh", "-c", "echo"}},
		{`[ "nginx", "-g", "daemon off;" ]`, []string{"nginx", "-g", "daemon off;"}},
		{`not an array`, nil},
		{`[broken`, nil},
	}
	for _, tc := range tests {
		got := ParseJSONArray(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("ParseJSONArray(%q) len = %d, want %d", tc.in, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseJSONArray(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseEnvAssign(t *testing.T) {
	tests := []struct {
		in   string
		k, v string
		ok   bool
	}{
		{"FOO=bar", "FOO", "bar", true},
		{"FOO bar", "FOO", "bar", true},
		{"FOO", "", "", false},
		{"PATH=/usr/bin:/bin", "PATH", "/usr/bin:/bin", true},
		{"GREET hello world", "GREET", "hello world", true},
	}
	for _, tc := range tests {
		k, v, ok := ParseEnvAssign(tc.in)
		if ok != tc.ok || k != tc.k || v != tc.v {
			t.Errorf("ParseEnvAssign(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, k, v, ok, tc.k, tc.v, tc.ok)
		}
	}
}
