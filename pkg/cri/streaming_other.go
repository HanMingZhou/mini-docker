//go:build !linux

package cri

import (
	"errors"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// StreamingConfig mirrors the linux configuration; the stub ignores it.
type StreamingConfig struct {
	Addr           string
	BaseURL        string
	IdleTO         interface{} // placeholder
	MydockerBinary string
}

type wrappedStreamingServer struct{}

func NewStreamingServer(_ string, _ StreamingConfig) (*wrappedStreamingServer, error) {
	return nil, errors.New("streaming server is only available on Linux")
}

func (w *wrappedStreamingServer) Start() error { return errors.New("not supported") }
func (w *wrappedStreamingServer) Stop() error  { return nil }
func (w *wrappedStreamingServer) GetExec(*runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return nil, errors.New("not supported")
}
func (w *wrappedStreamingServer) GetAttach(*runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return nil, errors.New("not supported")
}
func (w *wrappedStreamingServer) GetPortForward(*runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return nil, errors.New("not supported")
}
func (w *wrappedStreamingServer) Addr() string { return "" }
