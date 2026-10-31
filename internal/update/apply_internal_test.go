package update

import (
	"os"
	"path/filepath"
	"testing"
)

// The Windows rename-aside path, exercised on every OS.
func TestReplaceRenameAside(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "machine-monitor.exe")
	os.WriteFile(exe, []byte("v1"), 0o755)
	os.WriteFile(exe+".old", []byte("v0"), 0o755) // left by an earlier update

	if err := replace(exe, []byte("v2"), true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "v2" {
		t.Fatalf("exe = %q, want v2", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "v1" {
		t.Fatalf("exe.old = %q, want v1", b)
	}
	CleanOld(exe)
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf("CleanOld left %s.old", exe)
	}
}

func TestReplaceFailsWithoutLeavingTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if err := replace(filepath.Join(dir, "machine-monitor"), []byte("x"), false); err == nil {
		t.Fatal("replace into a missing directory succeeded")
	}
}
