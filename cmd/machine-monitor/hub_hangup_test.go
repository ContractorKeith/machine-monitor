package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

func TestHubSurvivesHangup(t *testing.T) {
	if os.Getenv("MM_HUP_HELPER") == "1" {
		os.Exit(runHub([]string{"--port", os.Getenv("MM_HUP_PORT")}, os.Stdout, os.Stderr))
	}
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX SIGHUP")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:")
	ln.Close()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHubSurvivesHangup$")
	cmd.Env = append(os.Environ(), "MM_HUP_HELPER=1", "MM_HUP_PORT="+port, "MACHINE_MONITOR_DIR="+dir)
	var out syncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	c := &http.Client{Timeout: time.Second}
	url := "http://127.0.0.1:" + port
	var key string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		key, err = hub.LocalKey(config.At(dir))
		if err != nil {
			continue
		}
		res, err := c.Get(url + "/api/info")
		if err == nil {
			res.Body.Close()
			break
		}
	}
	if key == "" {
		t.Fatalf("hub did not start: %s", out.String())
	}
	post := func(path, body string, auth bool) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest("POST", url+path, bytes.NewBufferString(body))
		if auth {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatalf("hub request: %v; output: %s", err, out.String())
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	_, b := post("/api/pair/code", "{}", true)
	var code struct{ Code string }
	if err := json.Unmarshal(b, &code); err != nil || code.Code == "" {
		t.Fatalf("pair code: %s", b)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if status, b := post("/api/pair", `{"code":"`+code.Code+`"}`, false); status != 200 {
		t.Fatalf("pair after HUP: %d %s", status, b)
	}
	if status, b := post("/api/report", `{"id":"paired","token":"`+key+`"}`, false); status != 200 {
		t.Fatalf("paired report: %d %s", status, b)
	}
	if status, _ := post("/api/report", `{"id":"unpaired"}`, false); status != 401 {
		t.Fatalf("unpaired report: %d", status)
	}
}
