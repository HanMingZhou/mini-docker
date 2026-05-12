package cri

import (
	"bytes"
	"context"
	"testing"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

func TestCapWriter(t *testing.T) {
	var buf bytes.Buffer
	w := capBuffer(&buf, 10)

	// First write under limit
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	// Second write crosses the cap
	n, err = w.Write([]byte("world!!!!!extra"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len("world!!!!!extra") {
		t.Errorf("want full input length %d, got %d", len("world!!!!!extra"), n)
	}
	// Buffer should contain exactly 10 bytes
	if buf.Len() != 10 {
		t.Errorf("buffer len = %d, want 10", buf.Len())
	}
	if buf.String() != "helloworld" {
		t.Errorf("buffer = %q, want %q", buf.String(), "helloworld")
	}

	// Third write after full is silently dropped
	n, err = w.Write([]byte("dropped"))
	if err != nil || n != len("dropped") {
		t.Errorf("third write: n=%d err=%v", n, err)
	}
	if buf.Len() != 10 {
		t.Errorf("buffer grew past cap: %d", buf.Len())
	}
}

func TestExecSyncValidation(t *testing.T) {
	root := t.TempDir()
	mgr, err := sandbox.NewManager(root, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := newRuntimeService(mgr)
	ctx := context.Background()

	// Missing container id
	_, err = svc.ExecSync(ctx, &runtime.ExecSyncRequest{})
	if err == nil {
		t.Fatal("expected error for missing container id")
	}

	// Missing cmd
	_, err = svc.ExecSync(ctx, &runtime.ExecSyncRequest{ContainerId: "abc"})
	if err == nil {
		t.Fatal("expected error for empty cmd")
	}

	// Unknown container
	_, err = svc.ExecSync(ctx, &runtime.ExecSyncRequest{
		ContainerId: "nonexistent",
		Cmd:         []string{"/bin/true"},
	})
	if err == nil {
		t.Fatal("expected error for missing container")
	}
}

func TestExecWithoutStreamingServer(t *testing.T) {
	root := t.TempDir()
	mgr, err := sandbox.NewManager(root, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := newRuntimeService(mgr)
	ctx := context.Background()

	// No streaming server configured -> clear error
	_, err = svc.Exec(ctx, &runtime.ExecRequest{ContainerId: "abc", Cmd: []string{"sh"}})
	if err == nil {
		t.Fatal("expected error when streaming not configured")
	}

	_, err = svc.Attach(ctx, &runtime.AttachRequest{ContainerId: "abc"})
	if err == nil {
		t.Fatal("expected error when streaming not configured")
	}

	_, err = svc.PortForward(ctx, &runtime.PortForwardRequest{PodSandboxId: "abc", Port: []int32{80}})
	if err == nil {
		t.Fatal("expected error when streaming not configured")
	}
}
