package tailscale

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseStatus(t *testing.T) {
	cases := []struct {
		name, status, serve string
		want                string
	}{
		{"running with serve", "running.json", "serve.json",
			`{"installed":true,"running":true,"state":"Running","ip":"100.101.102.103","dnsName":"studio.tail0000.ts.net",` +
				`"httpsUrl":"https://studio.tail0000.ts.net","urls":["https://studio.tail0000.ts.net","http://100.101.102.103:7777","http://studio.tail0000.ts.net:7777"],"tailnet":"example.com"}`},
		{"running, serve points elsewhere", "running.json", "serve-other.json",
			`{"installed":true,"running":true,"state":"Running","ip":"100.101.102.103","dnsName":"studio.tail0000.ts.net",` +
				`"httpsUrl":null,"urls":["http://100.101.102.103:7777","http://studio.tail0000.ts.net:7777"],"tailnet":"example.com"}`},
		{"running, no serve config", "running.json", "",
			`{"installed":true,"running":true,"state":"Running","ip":"100.101.102.103","dnsName":"studio.tail0000.ts.net",` +
				`"httpsUrl":null,"urls":["http://100.101.102.103:7777","http://studio.tail0000.ts.net:7777"],"tailnet":"example.com"}`},
		{"not logged in", "needs-login.json", "",
			`{"installed":true,"running":false,"state":"NeedsLogin","ip":null,"dnsName":null,"httpsUrl":null,"urls":[],"tailnet":null}`},
		{"stopped keeps identity, no urls", "stopped.json", "serve.json",
			`{"installed":true,"running":false,"state":"Stopped","ip":"100.101.102.103","dnsName":"studio.tail0000.ts.net","httpsUrl":null,"urls":[],"tailnet":"example.com"}`},
	}
	for _, c := range cases {
		var serve []byte
		if c.serve != "" {
			serve = fixture(t, c.serve)
		}
		if got := asJSON(t, parseStatus(fixture(t, c.status), serve, 7777)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestParseStatusDaemonDown(t *testing.T) {
	// tailscaled not running: the CLI prints an error to stderr and nothing to stdout.
	for _, out := range []string{"", "failed to connect to local tailscaled", "{"} {
		if got := asJSON(t, parseStatus([]byte(out), nil, 7777)); got != `{"installed":true,"running":false,"state":"NotRunning","ip":null,"dnsName":null,"httpsUrl":null,"urls":[],"tailnet":null}` {
			t.Errorf("%q: %s", out, got)
		}
	}
}

func TestNotInstalled(t *testing.T) {
	defer func(c func() [][]string) { candidates = c }(candidates)
	candidates = func() [][]string { return [][]string{{filepath.Join(t.TempDir(), "missing", "tailscale")}} }
	if Installed() {
		t.Fatal("Installed() with no CLI")
	}
	if got := asJSON(t, Read(context.Background(), 7777)); got != `{"installed":false,"running":false,"state":null,"ip":null,"dnsName":null,"httpsUrl":null,"urls":[],"tailnet":null}` {
		t.Errorf("Read: %s", got)
	}
	if got := PeerURLs(context.Background(), 7777); got != nil {
		t.Errorf("PeerURLs: %v", got)
	}
}

func TestServeURLRequiresHubPort(t *testing.T) {
	if u := serveURL(fixture(t, "serve.json"), 7777); u == nil || *u != "https://studio.tail0000.ts.net" {
		t.Errorf("port 7777: %v", u)
	}
	if u := serveURL(fixture(t, "serve.json"), 777); u != nil {
		t.Errorf("port 777 must not match 7777: %s", *u)
	}
	for _, proxy := range []string{"localhost:7777", "http://localhost:7777/", "https://127.0.0.1:7777"} {
		js := `{"Web":{"h.ts.net:8443":{"Handlers":{"/":{"Proxy":"` + proxy + `"}}}}}`
		if u := serveURL([]byte(js), 7777); u == nil || *u != "https://h.ts.net:8443" {
			t.Errorf("%s: %v", proxy, u)
		}
	}
}

func TestParsePeers(t *testing.T) {
	want := []string{"https://phone.tail0000.ts.net", "http://100.64.0.3:7777", "https://laptop.tail0000.ts.net", "http://100.64.0.2:7777"}
	if got := parsePeers(fixture(t, "running.json"), 7777); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got := parsePeers(fixture(t, "needs-login.json"), 7777); got != nil {
		t.Errorf("not logged in: %v", got)
	}
	// Capped at 40 URLs.
	var b strings.Builder
	b.WriteString(`{"BackendState":"Running","Peer":{`)
	for i := range 30 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":{"DNSName":"p.ts.net.","TailscaleIPs":["100.64.0.9"],"Online":true}`)
	}
	b.WriteString("}}")
	if got := parsePeers([]byte(b.String()), 7777); len(got) != maxPeers {
		t.Errorf("cap: %d urls", len(got))
	}
}

// A fake CLI checks that run passes args, strips the environment and keeps stdout on a
// non-zero exit.
func TestRunFakeCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI")
	}
	exe := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\n" +
		`if [ -n "$MM_SECRET" ]; then echo leaked; exit 0; fi` + "\n" +
		`if [ "$1 $2" = "status --json" ]; then cat ` + filepath.Join(mustAbs(t, "testdata"), "needs-login.json") + "; exit 1; fi\n" +
		"exit 2\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func(c func() [][]string) { candidates = c }(candidates)
	candidates = func() [][]string { return [][]string{{exe}} }
	t.Setenv("MM_SECRET", "x")
	if got := asJSON(t, Read(context.Background(), 7777)); got != `{"installed":true,"running":false,"state":"NeedsLogin","ip":null,"dnsName":null,"httpsUrl":null,"urls":[],"tailnet":null}` {
		t.Errorf("Read: %s", got)
	}
}

func mustAbs(t *testing.T, p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// With none of the allow-listed variables set, the child must still get an empty
// environment, not the parent's (a nil exec.Cmd.Env inherits everything).
func TestRunStripsEnvWithEmptyAllowList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI")
	}
	if env := strippedEnv(); env == nil {
		t.Fatal("strippedEnv() is nil")
	}
	exe := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\n" + `if [ -n "$MM_SENTINEL" ]; then echo leaked; else echo clean; fi` + "\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func(c func() [][]string) { candidates = c }(candidates)
	candidates = func() [][]string { return [][]string{{exe}} }
	for _, k := range allowedEnv {
		t.Setenv(k, "") // restores the original value after the test
		os.Unsetenv(k)
	}
	t.Setenv("MM_SENTINEL", "secret")
	if env := strippedEnv(); env == nil || len(env) != 0 {
		t.Fatalf("strippedEnv() = %#v, want empty non-nil", env)
	}
	if got := string(run(context.Background(), "status")); got != "clean\n" {
		t.Errorf("child saw parent env: %q", got)
	}
}
