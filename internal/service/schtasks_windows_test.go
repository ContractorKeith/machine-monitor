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
// this binary as `-test.run=^TestSchtasksHelperProcess$ <marker>`. It writes the marker
// file and stays up long enough for /Query to see the task running.
func TestSchtasksHelperProcess(t *testing.T) {
	if flag.NArg() != 1 {
		t.Skip("only runs as the scheduled task's payload")
	}
	os.WriteFile(flag.Arg(0), []byte("started"), 0o600)
	time.Sleep(20 * time.Second)
}

// TestSchtasksLogonTaskIntegration creates a real logon task through the same XML and
// schtasks calls as install, runs it once through conhost --headless, and deletes it.
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

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
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
}
