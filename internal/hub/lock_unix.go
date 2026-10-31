//go:build !windows

package hub

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var errLocked = errors.New("locked")

// lockFile takes a non-blocking exclusive flock on f.
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
