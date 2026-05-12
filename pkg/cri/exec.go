package cri

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/nsenter"
	"github.com/mini-docker/mini-docker/pkg/store"
)

// ExecSync runs a command inside a container synchronously and returns stdout,
// stderr and the exit code. Used for probes and short diagnostic commands.
func (s *RuntimeService) ExecSync(_ context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	if req.ContainerId == "" {
		return nil, errors.New("ExecSync: empty container_id")
	}
	if len(req.Cmd) == 0 {
		return nil, errors.New("ExecSync: empty cmd")
	}

	rec, err := resolveRunningContainer(s.root(), req.ContainerId)
	if err != nil {
		return nil, fmt.Errorf("ExecSync: %w", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	stdout := capBuffer(&stdoutBuf, maxExecSyncOutput)
	stderr := capBuffer(&stderrBuf, maxExecSyncOutput)

	done := make(chan execResult, 1)
	go func() {
		code, err := nsenter.Spawn(nsenter.ExecSpec{
			Target: nsenter.Target{
				TargetPID:  rec.PID,
				Namespaces: nsenter.DefaultNamespaces(),
			},
			Argv:       req.Cmd,
			Stdout:     stdout,
			Stderr:     stderr,
			InitBinary: s.mydockerBinary,
		})
		done <- execResult{code: code, err: err}
	}()

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	select {
	case res := <-done:
		if res.err != nil {
			return &runtime.ExecSyncResponse{
				Stdout:   stdoutBuf.Bytes(),
				Stderr:   append(stderrBuf.Bytes(), []byte(res.err.Error())...),
				ExitCode: 255,
			}, nil
		}
		return &runtime.ExecSyncResponse{
			Stdout:   stdoutBuf.Bytes(),
			Stderr:   stderrBuf.Bytes(),
			ExitCode: int32(res.code),
		}, nil
	case <-time.After(timeout):
		return &runtime.ExecSyncResponse{
			Stdout:   stdoutBuf.Bytes(),
			Stderr:   []byte(fmt.Sprintf("command timed out after %s", timeout)),
			ExitCode: 124, // conventional timeout exit code
		}, nil
	}
}

// Exec returns a streaming URL for stdio. Requires a running streaming server;
// if none is configured, falls back to Unimplemented so callers see a clear error.
func (s *RuntimeService) Exec(_ context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	if s.streaming == nil {
		return nil, errors.New("Exec: streaming server not configured")
	}
	return s.streaming.GetExec(req)
}

// Attach returns a streaming URL for attaching to a container's stdio.
func (s *RuntimeService) Attach(_ context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	if s.streaming == nil {
		return nil, errors.New("Attach: streaming server not configured")
	}
	return s.streaming.GetAttach(req)
}

// PortForward returns a streaming URL for port-forwarding into a pod sandbox.
func (s *RuntimeService) PortForward(_ context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	if s.streaming == nil {
		return nil, errors.New("PortForward: streaming server not configured")
	}
	return s.streaming.GetPortForward(req)
}

// --- helpers ----------------------------------------------------------------

type execResult struct {
	code int
	err  error
}

// maxExecSyncOutput caps captured stdout/stderr to avoid OOM on runaway commands.
// Kubelet probes produce tiny output; 16 MB is plenty.
const maxExecSyncOutput = 16 << 20

// capBuffer wraps an io.Writer, silently dropping bytes after max.
type capWriter struct {
	w       io.Writer
	max     int
	written int
}

func capBuffer(w io.Writer, max int) *capWriter {
	return &capWriter{w: w, max: max}
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.written >= c.max {
		return len(p), nil
	}
	remaining := c.max - c.written
	if len(p) > remaining {
		n, err := c.w.Write(p[:remaining])
		c.written += n
		if err != nil {
			return n, err
		}
		return len(p), nil // pretend we consumed the rest
	}
	n, err := c.w.Write(p)
	c.written += n
	return n, err
}

// resolveRunningContainer loads a container record and ensures it's running.
func resolveRunningContainer(root, ref string) (*store.Container, error) {
	st, err := store.New(root)
	if err != nil {
		return nil, err
	}
	rec, err := st.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if rec.State != store.StateRunning || !pidAlive(rec.PID) {
		return nil, fmt.Errorf("container %s is not running", rec.ID)
	}
	return rec, nil
}
