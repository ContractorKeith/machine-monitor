package service

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSchtasksHelperProcess is the task's payload, not a test: the integration test runs
// a copy of this binary as `-test.run=^TestSchtasksHelperProcess$ <heartbeat>`. It
// rewrites the heartbeat file every 200ms for up to a minute, so the test can see both
// that it started and that /End stopped it.
func TestSchtasksHelperProcess(t *testing.T) {
	if flag.NArg() != 1 {
		t.Skip("only runs as the scheduled task's payload")
	}
	for range 300 {
		os.WriteFile(flag.Arg(0), []byte(time.Now().Format(time.RFC3339Nano)), 0o600)
		time.Sleep(200 * time.Millisecond)
	}
}

// copyFile copies src to dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o700); err != nil {
		t.Fatal(err)
	}
}

// beating reports whether the heartbeat file changes within 2s.
func beating(path string) bool {
	first, _ := os.ReadFile(path)
	for range 10 {
		time.Sleep(200 * time.Millisecond)
		if b, _ := os.ReadFile(path); string(b) != string(first) {
			return true
		}
	}
	return false
}

// TestSchtasksLogonTaskIntegration creates a real logon task through the same XML and
// schtasks calls as install, runs it once, checks /End stops
// it (reinstall relies on that), and deletes it.
func TestSchtasksLogonTaskIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a real scheduled task")
	}
	m, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	m.DataDir = t.TempDir() // the scratch XML goes here, not in the real data dir
	if m.User == "" {
		t.Fatal("no current user")
	}
	ctx := context.Background()
	name := fmt.Sprintf("machine-monitor-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		m.run(ctx, "schtasks", "/End", "/TN", name)
		if _, err := m.run(ctx, "schtasks", "/Delete", "/TN", name, "/F"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Run a copy so a payload that outlives the test can't lock go test's own binary.
	// The space proves the quoted <Command> path works, as under C:\Program Files.
	dir := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "payload.exe")
	copyFile(t, self, exe)
	marker := filepath.Join(dir, "heartbeat")
	xml := m.taskXML("machine-monitor integration test", []string{exe, "-test.run=^TestSchtasksHelperProcess$", marker})
	if err := m.createTask(ctx, name, xml); err != nil {
		t.Fatalf("create: %v\n%s", err, xml)
	}
	if ok, state := m.queryTask(ctx, name); !ok {
		t.Fatal("/Query doesn't find the new task")
	} else {
		t.Logf("after create: %s", state)
	}
	if _, err := m.run(ctx, "schtasks", "/Run", "/TN", name); err != nil {
		t.Fatalf("run: %v", err)
	}

	started, sawRunning := false, false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !(started && sawRunning); time.Sleep(500 * time.Millisecond) {
		if !started && fileExists(marker) {
			started = true
		}
		if _, state := m.queryTask(ctx, name); state == "Running" {
			sawRunning = true
		}
	}
	if !started {
		_, state := m.queryTask(ctx, name)
		verbose, _ := m.run(ctx, "schtasks", "/Query", "/TN", name, "/FO", "LIST", "/V")
		t.Fatalf("the task's command never started (status %q)\n%s\n%s", state, verbose, xml)
	}
	if !sawRunning {
		t.Error("/Query never reported the task as Running while its command ran")
	}

	if _, err := m.run(ctx, "schtasks", "/End", "/TN", name); err != nil {
		t.Fatalf("end: %v", err)
	}
	stopped := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if !beating(marker) {
			stopped = true
			break
		}
	}
	if !stopped {
		t.Error("/End left the task's command running")
	}
	if _, state := m.queryTask(ctx, name); state == "Running" {
		t.Errorf("/Query still reports Running after /End")
	}
}
