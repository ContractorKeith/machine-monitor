package hub

import (
	"encoding/json"
	"testing"
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
