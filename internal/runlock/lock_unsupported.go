//go:build !unix && !windows

package runlock

import "os"

func platformSupported() error {
	return ErrUnsupported
}

func lockFile(*os.File, bool) error {
	return ErrUnsupported
}

func unlockFile(*os.File) error {
	return nil
}
