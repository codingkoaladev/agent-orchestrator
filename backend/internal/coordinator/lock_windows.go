//go:build windows

package coordinator

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset places the locked byte past the PID text so a contender can still
// read the holder's PID for diagnostics.
const lockOffset = 1 << 30

func tryRunLock(file *os.File) error {
	overlapped := &windows.Overlapped{Offset: lockOffset}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errRunLocked
	}
	return err
}

func unlockRunLock(file *os.File) error {
	overlapped := &windows.Overlapped{Offset: lockOffset}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
}
