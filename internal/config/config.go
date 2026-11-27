package config

import (
	"os"
	"path/filepath"
)

const DirEnv = "MACHINE_MONITOR_DIR"

type Paths struct {
	Dir       string
	Config    string
	Devices   string
	Alerts    string
	AlertsLog string
	History   string
	Client    string
}

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

func WriteFile0600(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
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
