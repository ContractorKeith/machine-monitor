//go:build windows

package hub

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

var errLocked = errors.New("locked")

// lockFile takes a non-blocking exclusive lock on all of f.
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLocked
	}
	return err
}
