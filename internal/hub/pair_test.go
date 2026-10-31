package hub

import (
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

func (th *testHub) pair(code string) (int, map[string]string) {
	th.t.Helper()
	w := th.req("POST", "/api/pair", "", fmt.Sprintf(`{"code":%q}`, code))
	return w.Code, decode[map[string]string](th.t, w)
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
	if code, body := th.pair("123456"); code != 403 || body["token"] != "" {
		t.Fatalf("no active code: %d %v", code, body)
	}
	c := th.newCode()
	// Spaces and dashes are ignored.
	code, body := th.pair(c[:3] + " - " + c[3:])
	if code != 200 || body["token"] != th.Token() {
		t.Fatalf("pair: %d %v", code, body)
	}
	if code, body := th.pair(c); code != 403 || body["token"] != "" {
		t.Fatalf("reused code: %d %v", code, body)
	}
}

func TestPairCodeExpires(t *testing.T) {
	th := newTestHub(t)
	c := th.newCode()
	th.clk.Add(PairTTL - time.Second)
	th.clk.Add(2 * time.Second)
	if code, body := th.pair(c); code != 403 || body["token"] != "" {
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
		if i == PairTries && body["error"] != "Wrong pairing code. Too many tries: get a new code from the hub." {
			t.Fatalf("last try: %q", body["error"])
		}
	}
	if code, body := th.pair(c); code != 403 || body["token"] != "" {
		t.Fatalf("right code after %d wrong tries: %d %v", PairTries, code, body)
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
