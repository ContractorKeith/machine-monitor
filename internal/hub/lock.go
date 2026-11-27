package hub

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var errDirInUse = errors.New("hub: another machine-monitor hub is already using this data directory; " +
	"stop it first, or give this one its own with MACHINE_MONITOR_DIR")

func lockDir(dir string) (io.Closer, error) {
	f, err := os.OpenFile(filepath.Join(dir, "hub.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
			return nil, errDirInUse
		}
		return nil, fmt.Errorf("hub: lock %s: %w", f.Name(), err)
	}
	return f, nil
}
