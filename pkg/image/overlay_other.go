//go:build !linux

package image

import "errors"

func prepareRootfs(_ *Store, _, _ string) (string, error) {
	return "", errors.New("image.PrepareRootfs only works on Linux")
}

func cleanupRootfs(_ string) error {
	return errors.New("image.CleanupRootfs only works on Linux")
}
