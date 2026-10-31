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

// Through a trusted proxy (tailscale serve) each forwarded client has its own tries.
func TestPairTriesUseForwardedClient(t *testing.T) {
	th := newTestHub(t)
	th.mu.Lock()
	th.cfg.extra = map[string]json.RawMessage{"trustedProxies": json.RawMessage(`["127.0.0.1"]`)}
	th.mu.Unlock()
	c := th.newCode()
	try := func(fwd, code string) int {
		return th.req("POST", "/api/pair", "", fmt.Sprintf(`{"code":%q}`, code), "X-Forwarded-For", fwd).Code
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
