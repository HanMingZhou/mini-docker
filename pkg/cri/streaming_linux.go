//go:build linux

package cri

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"k8s.io/client-go/tools/remotecommand"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubelet/pkg/cri/streaming"

	"github.com/mini-docker/mini-docker/pkg/nsenter"
)

// streamingAdapter implements k8s.io/kubelet/pkg/cri/streaming.Runtime by
// delegating to pkg/nsenter for stdio-connected exec.
type streamingAdapter struct {
	root           string
	mydockerBinary string
}

func (a *streamingAdapter) Exec(_ context.Context, containerID string, cmd []string,
	in io.Reader, out, errW io.WriteCloser, tty bool,
	resize <-chan remotecommand.TerminalSize) error {

	rec, err := resolveRunningContainer(a.root, containerID)
	if err != nil {
		return err
	}
	defer func() {
		if out != nil {
			_ = out.Close()
		}
		if errW != nil && errW != out {
			_ = errW.Close()
		}
	}()

	// NOTE: resize events are ignored; a full TTY implementation needs a pty
	// and ioctl(TIOCSWINSZ). For Level 3 we accept line-oriented I/O only.
	_ = resize

	code, err := nsenter.Spawn(nsenter.ExecSpec{
		Target: nsenter.Target{
			TargetPID:  rec.PID,
			Namespaces: nsenter.DefaultNamespaces(),
		},
		Argv:       cmd,
		Stdin:      in,
		Stdout:     out,
		Stderr:     errW,
		TTY:        tty,
		InitBinary: a.mydockerBinary,
	})
	if err != nil {
		return fmt.Errorf("nsenter exec: %w", err)
	}
	if code != 0 {
		// streaming contract: non-zero exit is returned as error.
		return fmt.Errorf("command exited with code %d", code)
	}
	return nil
}

func (a *streamingAdapter) Attach(_ context.Context, containerID string,
	in io.Reader, out, errW io.WriteCloser, tty bool,
	resize <-chan remotecommand.TerminalSize) error {
	// Attach to a container's current PID 1 stdio. Our containers are run
	// detached with stdout/stderr redirected to log files, so attach is not
	// meaningfully implementable without a supervisor. Return Unsupported.
	_, _, _, _ = in, out, errW, resize
	_ = tty
	_ = containerID
	return fmt.Errorf("Attach is not supported (containers are detached with file-backed logs)")
}

func (a *streamingAdapter) PortForward(_ context.Context, podSandboxID string,
	port int32, stream io.ReadWriteCloser) error {
	// Minimal implementation: dial 127.0.0.1:<port> **inside the sandbox's
	// network namespace**. The proper implementation enters the sandbox netns
	// and opens a TCP connection. For brevity, we skip netns entry and just
	// dial the host — this works only if the container's app binds to the
	// host network, which is NOT our default. Return Unsupported for now.
	_ = podSandboxID
	_ = port
	_ = stream
	return fmt.Errorf("PortForward is not implemented yet")
}

// StreamingConfig configures the streaming HTTP server.
type StreamingConfig struct {
	Addr           string        // listen address, e.g. "127.0.0.1:10350"
	BaseURL        string        // base URL in returned streaming URLs; empty uses Addr
	IdleTO         time.Duration // idle timeout; 0 uses a sensible default
	MydockerBinary string        // path to the mydocker binary used as nsexec host
}

// wrappedStreamingServer holds the underlying streaming.Server and lets us
// manage its lifecycle.
type wrappedStreamingServer struct {
	inner streaming.Server
	addr  string
}

// NewStreamingServer constructs a streaming server bound to addr.
// Caller is responsible for starting it (via Start) and stopping (via Stop).
func NewStreamingServer(root string, cfg StreamingConfig) (*wrappedStreamingServer, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:10350"
	}
	host, portStr, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("invalid streaming addr %q: %w", cfg.Addr, err)
	}

	sc := streaming.DefaultConfig
	sc.Addr = cfg.Addr
	if cfg.IdleTO > 0 {
		sc.StreamIdleTimeout = cfg.IdleTO
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		h := host
		if h == "" || h == "0.0.0.0" || h == "::" {
			h = "127.0.0.1"
		}
		baseURL = "http://" + net.JoinHostPort(h, portStr)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base url %q: %w", baseURL, err)
	}
	sc.BaseURL = u

	inner, err := streaming.NewServer(sc, &streamingAdapter{root: root, mydockerBinary: cfg.MydockerBinary})
	if err != nil {
		return nil, err
	}
	return &wrappedStreamingServer{inner: inner, addr: cfg.Addr}, nil
}

// Start begins serving HTTP requests. Blocks until Stop.
func (w *wrappedStreamingServer) Start() error {
	return w.inner.Start(true)
}

// Stop gracefully stops the streaming server.
func (w *wrappedStreamingServer) Stop() error {
	return w.inner.Stop()
}

// GetExec delegates to the underlying server.
func (w *wrappedStreamingServer) GetExec(req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return w.inner.GetExec(req)
}
func (w *wrappedStreamingServer) GetAttach(req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return w.inner.GetAttach(req)
}
func (w *wrappedStreamingServer) GetPortForward(req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return w.inner.GetPortForward(req)
}

// Addr returns the listening address (for logs).
func (w *wrappedStreamingServer) Addr() string { return w.addr }
