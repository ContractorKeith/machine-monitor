// Package config locates machine-monitor's data directory and writes private files in it.
//
// Layout (PLAN §5), under os.UserConfigDir()/machine-monitor:
//
//	config.json       hub token, energy settings, allowedHosts
//	devices.json      device registry
//	alerts.json       alert rules and channels
//	alerts-log.json   alert event log
//	history/<id>.ndjson
//	client.json       hub URL and key, for agents and connected TUIs
//
// Every file is written with WriteFile0600. Setting MACHINE_MONITOR_DIR overrides the
// directory, which lets tests and a second instance on one machine keep separate data.
package config

import (
	"os"
	"path/filepath"
)

// DirEnv overrides the data directory when set.
const DirEnv = "MACHINE_MONITOR_DIR"

// Paths is every file and directory machine-monitor keeps.
type Paths struct {
	Dir       string
	Config    string
	Devices   string
	Alerts    string
	AlertsLog string
	History   string // directory of <id>.ndjson files
	Client    string
}

// Default returns the paths under $MACHINE_MONITOR_DIR, or os.UserConfigDir()/machine-monitor
// (%APPDATA% on Windows, ~/Library/Application Support on macOS, ~/.config on Linux).
func Default() (Paths, error) {
	if d := os.Getenv(DirEnv); d != "" {
		return At(d), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	return At(filepath.Join(base, "machine-monitor")), nil
}

// At returns the paths under dir. It does not create anything.
func At(dir string) Paths {
	return Paths{
		Dir:       dir,
		Config:    filepath.Join(dir, "config.json"),
		Devices:   filepath.Join(dir, "devices.json"),
		Alerts:    filepath.Join(dir, "alerts.json"),
		AlertsLog: filepath.Join(dir, "alerts-log.json"),
		History:   filepath.Join(dir, "history"),
		Client:    filepath.Join(dir, "client.json"),
	}
}

// WriteFile0600 replaces path with data atomically: it writes a temp file in the same
// directory with mode 0600, syncs it, and renames it over path, so a crash never leaves a
// half-written file. Missing parent directories are created with mode 0700. On Windows the
// mode bits are ignored; the file inherits the user profile's ACL.
func WriteFile0600(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*") // created 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
