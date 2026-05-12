package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootCommandStructure(t *testing.T) {
	root := newRootCmd()

	wantSubs := []string{"run", "exec", "ps", "logs", "stop", "rm", "image", "init", "nsexec"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range wantSubs {
		if !have[w] {
			t.Errorf("missing subcommand %q", w)
		}
	}

	// init 和 nsexec 必须隐藏
	for _, c := range root.Commands() {
		if (c.Name() == "init" || c.Name() == "nsexec") && !c.Hidden {
			t.Errorf("%s subcommand must be hidden", c.Name())
		}
	}
}

func TestRunCmdFlagAliases(t *testing.T) {
	root := newRootCmd()
	// --help 不触发 RunE，用它验证 cobra 能识别 `run -v /a:/b -e K=V`
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"run", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	// 确认我们期望的 flag 都出现在 help 中
	for _, want := range []string{"--image", "--rootfs", "--memory", "--cpus", "--volume", "--env", "-d,", "-t,"} {
		if !strings.Contains(out, want) {
			t.Errorf("run --help missing %q\nhelp:\n%s", want, out)
		}
	}
}
