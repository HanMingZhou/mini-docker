//go:build !linux

package container

import "errors"

func start(_ Config) (*Handle, error) {
	return nil, errors.New("container.Start only works on Linux")
}

func (h *Handle) wait() (int, error) {
	return -1, errors.New("container.Handle.Wait only works on Linux")
}

func (h *Handle) release() error {
	return errors.New("container.Handle.Release only works on Linux")
}

func initProcess() error {
	return errors.New("container.Init only works on Linux")
}
