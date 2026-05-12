//go:build !linux

package sandbox

import "errors"

func (m *Manager) start(_ Metadata, _ StartOptions) (*Sandbox, error) {
	return nil, errors.New("sandbox.Start only works on Linux")
}

func (m *Manager) stop(_ string) error {
	return errors.New("sandbox.Stop only works on Linux")
}

func pidAlive(_ int) bool { return false }
