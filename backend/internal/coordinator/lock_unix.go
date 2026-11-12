//go:build unix

package coordinator

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryRunLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errRunLocked
	}
	return err
}

func unlockRunLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
