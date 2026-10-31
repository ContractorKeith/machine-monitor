package hub

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

// newCode asks the hub for a pairing code with the key.
func (th *testHub) newCode() string {
	th.t.Helper()
	w := th.keyed("POST", "/api/pair/code", "")
	if w.Code != 200 {
		th.t.Fatalf("pair/code: %d %s", w.Code, w.Body)
	}
	v := decode[struct {
		Code    string `json:"code"`
		Expires int64  `json:"expires"`
	}](th.t, w)
	if !regexp.MustCompile(`^\d{6}$`).MatchString(v.Code) {
		th.t.Fatalf("code %q is not 6 digits", v.Code)
	}
	if want := th.clk.Now().Add(PairTTL).UnixMilli(); v.Expires != want {
		th.t.Fatalf("expires %d, want %d", v.Expires, want)
	}
	return v.Code
}

func (th *testHub) pair(code string) (int, map[string]any) {
	th.t.Helper()
	return th.pairFrom("127.0.0.1:50000", code)
}

// pairFrom tries code from the address remote.
func (th *testHub) pairFrom(remote, code string) (int, map[string]any) {
	th.t.Helper()
	w := th.req("POST", "/api/pair", "", fmt.Sprintf(`{"code":%q}`, code), "RemoteAddr", remote)
	return w.Code, decode[map[string]any](th.t, w)
}

// wrongCode is a code that differs from code.
func wrongCode(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

func TestPairCodeNeedsKey(t *testing.T) {
	th := newTestHub(t)
	if w := th.req("POST", "/api/pair/code", "", ""); w.Code != 401 {
		t.Fatalf("pair/code without key: %d", w.Code)
	}
}

func TestPairGivesKeyOnce(t *testing.T) {
	th := newTestHub(t)
	if code, body := th.pair("123456"); code != 403 || body["token"] != nil {
		t.Fatalf("no active code: %d %v", code, body)
	}
	c := th.newCode()
	// Spaces and dashes are ignored.
	code, body := th.pair(c[:3] + " - " + c[3:])
	if code != 200 || body["token"] != th.Token() {
		t.Fatalf("pair: %d %v", code, body)
	}
	if code, body := th.pair(c); code != 403 || body["token"] != nil {
		t.Fatalf("reused code: %d %v", code, body)
	}
}

func TestPairCodeExpires(t *testing.T) {
	th := newTestHub(t)
	c := th.newCode()
	th.clk.Add(PairTTL - time.Second)
	th.clk.Add(2 * time.Second)
	if code, body := th.pair(c); code != 403 || body["token"] != nil {
		t.Fatalf("expired code: %d %v", code, body)
	}

	c = th.newCode()
	th.clk.Add(PairTTL) // exactly at the deadline still works, as upstream (now > expires)
	if code, _ := th.pair(c); code != 200 {
		t.Fatalf("code at deadline: %d", code)
	}
}

func TestPairCodeDiesAfterFiveTries(t *testing.T) {
	th := newTestHub(t)
	c := th.newCode()
	for i := 1; i <= PairTries; i++ {
		code, body := th.pair(wrongCode(c))
		if code != 403 {
			t.Fatalf("try %d: %d", i, code)
		}
		if want := float64(PairTries - i); body["triesLeft"] != want || body["retry"] != (i < PairTries) {
			t.Fatalf("try %d: retry %v, triesLeft %v", i, body["retry"], body["triesLeft"])
		}
		if i == PairTries && body["error"] != "Wrong pairing code. Too many tries: get a new code from the hub." {
			t.Fatalf("last try: %q", body["error"])
		}
	}
	if code, body := th.pair(c); code != 403 || body["token"] != nil {
		t.Fatalf("right code after %d wrong tries: %d %v", PairTries, code, body)
	}
	// The tries are this source's: another machine can still use the code.
	if code, body := th.pairFrom("192.168.1.9:4000", c); code != 200 || body["token"] != th.Token() {
		t.Fatalf("another source after one burned its tries: %d %v", code, body)
	}

	// A new code resets the count; four wrong tries still leave it alive.
	c = th.newCode()
	for range PairTries - 1 {
		th.pair(wrongCode(c))
	}
	if code, _ := th.pair(c); code != 200 {
		t.Fatalf("right code after %d wrong tries: %d", PairTries-1, code)
	}
}

func TestNewCodeReplacesOld(t *testing.T) {
	th := newTestHub(t)
	old := th.newCode()
	var c string
	for c = th.newCode(); c == old; c = th.newCode() { // 1 in a million
	}
	if code, _ := th.pair(old); code != 403 {
		t.Fatalf("old code: %d", code)
	}
	if code, _ := th.pair(c); code != 200 {
		t.Fatalf("new code: %d", code)
	}
}

func TestPairIsRateLimited(t *testing.T) {
	th := newTestHub(t)
	for i := range RateMax {
		if w := th.req("POST", "/api/pair", "", `{}`); w.Code != 403 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if w := th.req("POST", "/api/pair", "", `{}`); w.Code != 429 {
		t.Fatalf("over the limit: %d", w.Code)
	}
	// Another address has its own allowance.
	if w := th.req("POST", "/api/pair", "", `{}`, "RemoteAddr", "192.168.1.9:4000"); w.Code != 403 {
		t.Fatalf("other address: %d", w.Code)
	}
	th.clk.Add(RateWindow + time.Second)
	if w := th.req("POST", "/api/pair", "", `{}`); w.Code != 403 {
		t.Fatalf("after the window: %d", w.Code)
	}
}

func TestPairBroadcastsPaired(t *testing.T) {
	th := newTestHub(t)
	srv := httptest.NewServer(th.h)
	t.Cleanup(srv.Close) // after the stream body closes (cleanups run last-in first-out)
	s := openStream(t, srv, th.Token())
	s.next("snapshot")
	c := th.newCode()
	if code, _ := th.pair(c); code != 200 {
		t.Fatal("pair failed")
	}
	s.next("paired")
}

// An IPv6 source is its /64, since a machine can use any address in it.
func TestPairTriesPerIPv6Prefix(t *testing.T) {
	th := newTestHub(t)
	c := th.newCode()
	for i := range PairTries {
		th.pairFrom(fmt.Sprintf("[fd12:3456::%x]:4000", i+1), wrongCode(c))
	}
	if code, _ := th.pairFrom("[fd12:3456::99]:4000", c); code != 403 {
		t.Fatalf("same /64 after %d wrong tries: %d", PairTries, code)
	}
	if code, _ := th.pairFrom("[fd12:3457::1]:4000", c); code != 200 {
		t.Fatalf("another /64: %d", code)
	}
}

// Many sources together still kill the code after pairMaxWrong wrong guesses.
func TestPairCodeDiesAfterMaxWrong(t *testing.T) {
	th := newTestHub(t)
	c := th.newCode()
	for i := range pairMaxWrong {
		code, _ := th.pairFrom(fmt.Sprintf("10.0.%d.%d:4000", i/PairTries/250, i/PairTries%250+1), wrongCode(c))
		if code != 403 {
			t.Fatalf("guess %d: %d", i+1, code)
		}
	}
	if code, body := th.pairFrom("192.168.7.7:4000", c); code != 403 || body["token"] != nil {
		t.Fatalf("right code after %d wrong guesses in all: %d %v", pairMaxWrong, code, body)
	}
}

// Through a trusted proxy each forwarded client has its own tries.
func TestPairTriesUseForwardedClient(t *testing.T) {
	th := newTestHub(t)
	th.mu.Lock()
	th.cfg.extra = map[string]json.RawMessage{"trustedProxies": json.RawMessage(`["192.168.1.2"]`)}
	th.mu.Unlock()
	c := th.newCode()
	try := func(fwd, code string) int {
		return th.req("POST", "/api/pair", "", fmt.Sprintf(`{"code":%q}`, code), "RemoteAddr", "192.168.1.2:5000", "X-Forwarded-For", fwd).Code
	}
	for i := range PairTries { // a spoofed leftmost entry doesn't buy fresh tries
		try(fmt.Sprintf("10.0.0.%d, 100.64.0.7", i+1), wrongCode(c))
	}
	if code := try("10.0.0.99, 100.64.0.7", c); code != 403 {
		t.Fatalf("same client behind a new spoofed entry: %d", code)
	}
	if code := try("100.64.0.8", c); code != 200 {
		t.Fatalf("another tailnet client: %d", code)
	}
}

// One live code at a time: {"replace": false}, as `machine-monitor pair` sends, is refused
// while a code is active, so two runs can't each show a code of which only one works.
// Without it a new code replaces the old one and says so with a "pairEnded" event.
func TestPairCodeOneAtATime(t *testing.T) {
	th := newTestHub(t)
	srv := httptest.NewServer(th.h)
	t.Cleanup(srv.Close)
	s := openStream(t, srv, th.Token())
	s.next("snapshot")
	first := decode[map[string]any](t, th.keyed("POST", "/api/pair/code", `{"replace":false}`))
	w := th.keyed("POST", "/api/pair/code", `{"replace":false}`)
	got := decode[map[string]any](t, w)
	if w.Code != 409 || got["active"] != true || got["expires"] != first["expires"] || got["code"] != nil {
		t.Fatalf("second exclusive code: %d %v", w.Code, got)
	}
	// Upstream clients (and the TUI's "new code") replace, and the first code's holder hears.
	second := decode[map[string]any](t, th.keyed("POST", "/api/pair/code", ""))
	if second["id"] == first["id"] {
		t.Fatalf("same id for a new code: %v", second["id"])
	}
	if ev := s.next("pairEnded"); ev["id"] != first["id"] || ev["reason"] != "replaced" {
		t.Fatalf("pairEnded = %v, want id %v", ev, first["id"])
	}
	if code, _ := th.pair(first["code"].(string)); code != 403 {
		t.Fatalf("replaced code: %d", code)
	}
	// "paired" names the code used.
	if code, _ := th.pair(second["code"].(string)); code != 200 {
		t.Fatal("pair failed")
	}
	if ev := s.next("paired"); ev["id"] != second["id"] {
		t.Fatalf("paired = %v, want id %v", ev, second["id"])
	}
	// Once used, a new exclusive code is fine.
	third := decode[map[string]any](t, th.keyed("POST", "/api/pair/code", `{"replace":false}`))
	// Cancelling another id leaves the code alone; its own id ends it.
	if w := th.keyed("DELETE", fmt.Sprintf("/api/pair/code?id=%v", second["id"]), ""); w.Code != 200 || decode[map[string]any](t, w)["ok"] != false {
		t.Fatalf("cancel of a used code: %d %s", w.Code, w.Body)
	}
	if w := th.keyed("DELETE", fmt.Sprintf("/api/pair/code?id=%v", third["id"]), ""); w.Code != 200 || decode[map[string]any](t, w)["ok"] != true {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if ev := s.next("pairEnded"); ev["id"] != third["id"] || ev["reason"] != "cancelled" {
		t.Fatalf("pairEnded = %v", ev)
	}
	if code, _ := th.pair(third["code"].(string)); code != 403 {
		t.Fatalf("cancelled code: %d", code)
	}
	if w := th.req("DELETE", "/api/pair/code?id=1", "", ""); w.Code != 401 {
		t.Fatalf("cancel without key: %d", w.Code)
	}
	// An expired code doesn't block a new one.
	th.keyed("POST", "/api/pair/code", `{"replace":false}`)
	th.clk.Add(PairTTL + time.Second)
	if w := th.keyed("POST", "/api/pair/code", `{"replace":false}`); w.Code != 200 {
		t.Fatalf("after expiry: %d %s", w.Code, w.Body)
	}
}

// A full hub refuses to pair, in a sentence, rather than pair a device that can never report.
func TestPairRefusedWhenFull(t *testing.T) {
	th := newTestHub(t)
	for i := range MaxDevices - 1 {
		th.postReport(fmt.Sprintf("d%d", i), nil)
	}
	c := th.newCode() // issued with one slot left
	th.postReport("last", nil)
	code, body := th.pair(c)
	if code != 409 || body["token"] != nil || body["error"] != fullMessage {
		t.Fatalf("pair on a full hub: %d %v", code, body)
	}
	w := th.keyed("POST", "/api/pair/code", "")
	if body := decode[map[string]any](t, w); w.Code != 409 || body["error"] != fullMessage {
		t.Fatalf("new code on a full hub: %d %v", w.Code, body)
	}
	// Removing a device makes room; the refused attempt didn't count as a guess.
	th.keyed("DELETE", "/api/devices/d0", "")
	if code, body := th.pair(c); code != 200 || body["token"] != th.Token() {
		t.Fatalf("after removing one: %d %v", code, body)
	}
}
