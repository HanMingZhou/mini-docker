//go:build !linux

package nsenter

import "errors"

func SpawnViaSelf(_ Target, _ []string, _ bool) (int, error) {
	return -1, errors.New("nsenter.SpawnViaSelf only works on Linux")
}

func Spawn(_ ExecSpec) (int, error) {
	return -1, errors.New("nsenter.Spawn only works on Linux")
}

func EnterAndExec() error {
	return errors.New("nsenter.EnterAndExec only works on Linux")
}
