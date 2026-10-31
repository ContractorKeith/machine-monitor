package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultHonorsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != dir || p.Config != filepath.Join(dir, "config.json") || p.History != filepath.Join(dir, "history") {
		t.Errorf("Default() = %+v", p)
	}
}

func TestDefaultUnderUserConfigDir(t *testing.T) {
	t.Setenv(DirEnv, "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skip("no user config dir:", err)
	}
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "machine-monitor"); p.Dir != want {
		t.Errorf("Dir = %q, want %q", p.Dir, want)
	}
}

func TestWriteFile0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "config.json")

	for _, body := range []string{`{"a":1}`, `{"b":2}`} { // create, then replace
		if err := WriteFile0600(path, []byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("read %q, %v; want %q", got, err, body)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file mode = %o, want 600", perm)
		}
		dinfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := dinfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("dir mode = %o, want 700", perm)
		}
	}
}
