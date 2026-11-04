package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeviceEnrollment(t *testing.T) {
	th := newTestHub(t)
	th.report("dev-1", nil) // legacy reports work
	enroll := func(id string) string {
		w := th.keyed("POST", "/api/pair/code", "{}")
		code := decode[map[string]any](t, w)["code"]
		b, _ := json.Marshal(map[string]any{"code": code, "deviceId": id})
		w = th.req("POST", "/api/pair", "", string(b))
		if w.Code != 200 {
			t.Fatalf("pair: %d", w.Code)
		}
		key := decode[map[string]string](t, w)["deviceKey"]
		if len(key) != 64 {
			t.Fatal("missing device key")
		}
		return key
	}
	key := enroll("dev-1")
	other := enroll("dev-2")
	for _, k := range []string{"", other, th.Token()} {
		w := th.req("POST", "/api/report", "", th.agentReport("dev-1", func(m map[string]any) { m["deviceKey"] = k }))
		if w.Code != 401 {
			t.Fatalf("wrong credential accepted: %d", w.Code)
		}
	}
	send := func(k string) reportReply {
		return th.report("dev-1", func(m map[string]any) { m["deviceKey"] = k; allowing("restart")(m) })
	}
	send(key)
	if code, _ := th.action("dev-1", "restart"); code != 200 {
		t.Fatal("action failed")
	}
	commands := send(key).Commands
	if len(commands) != 1 {
		t.Fatal("command missing")
	}
	c := commands[0]
	if c.Sig != SignCommand(key, "dev-1", c.ID, c.Action, c.T) {
		t.Fatal("wrong signing key")
	}
	if c.Sig == SignCommand(th.Token(), "dev-1", c.ID, c.Action, c.T) {
		t.Fatal("shared token signature")
	}
	if len(send(key).Commands) != 0 {
		t.Fatal("command delivered twice")
	}
	rotated := enroll("dev-1")
	if rotated == key {
		t.Fatal("key not rotated")
	}
	w := th.req("POST", "/api/report", "", th.agentReport("dev-1", func(m map[string]any) { m["deviceKey"] = key }))
	if w.Code != 401 {
		t.Fatal("old key accepted")
	}
	send(rotated)
	cfg, err := loadConfig(th.opt.Paths.Config)
	if err != nil || cfg.DeviceKeys["dev-1"] != rotated {
		t.Fatal("device keys not persisted")
	}
	th.report("legacy", nil)
}

// The hub's own report never crosses the network, so enrolling its own id (connect run on
// the hub's machine) must not stop it monitoring itself.
func TestOwnReportIgnoresDeviceKey(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	h, err := New(Options{Paths: config.At(t.TempDir()), Hostname: "testbox", Collector: fakeCollector{"hub-self"},
		Interval: 5 * time.Millisecond, Logf: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.cfg.DeviceKeys = map[string]string{"hub-self": strings.Repeat("a", 64)}
	h.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	h.Close()
	mu.Lock()
	defer mu.Unlock()
	for _, l := range logs {
		if strings.Contains(l, "bad device key") {
			t.Fatalf("own report refused: %q", l)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.latest["hub-self"] == nil {
		t.Fatal("hub never reported itself")
	}
}

// A damaged deviceKeys value names the file and the fix, not a bare json error.
func TestCorruptDeviceKeysNamesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"token":"abc","deviceKeys":"oops"}`), 0o600)
	_, err := loadConfig(p)
	if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "deviceKeys") ||
		!strings.Contains(err.Error(), "remove") {
		t.Fatalf("error = %v", err)
	}
}

// A fresh hub writes no "deviceKeys": null.
func TestFreshConfigHasNoNullDeviceKeys(t *testing.T) {
	th := newTestHub(t)
	b, err := os.ReadFile(th.opt.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("config.json has null: %s", b)
	}
}
