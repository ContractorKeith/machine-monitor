package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
)

func init() { built["export"] = true }

// TestExport runs export against internal/export's fixture history directory.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	paths := config.At(dir)
	if err := os.MkdirAll(paths.History, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "..", "internal", "export", "testdata", "history", "box-1.ndjson")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.History, "box-1.ndjson"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	exportNow = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	defer func() { exportNow = time.Now }()
	local := time.Local
	time.Local = time.UTC
	defer func() { time.Local = local }()

	tests := []runCase{
		{[]string{"export", "box-1", "--range", "7d"}, 0, "2026-10-31T10:01:00Z,80,97.5,", ""},
		{[]string{"export", "--format", "json", "box-1", "--range", "30d"}, 0, `"time": "2026-10-20T08:00:00Z"`, ""},
		{[]string{"export", "box-1", "--from", "2026-10-31T10:01:00Z", "--to", "2026-10-31T10:01:30Z"}, 0, "\n2026-10-31T10:01:00Z,", ""},
		{[]string{"export"}, 2, "", "usage: machine-monitor export ID"},
		{[]string{"export", "a", "b"}, 2, "", "usage: machine-monitor export ID"},
		{[]string{"export", "ghost"}, 1, "", `machine-monitor export: There is no device "ghost" in this machine's history.`},
		{[]string{"export", "box-1", "--range", "1h"}, 1, "", `has no history between 2026-10-31T11:00:00Z and 2026-10-31T12:00:00Z.`},
		{[]string{"export", "box-1", "--range", "2w"}, 1, "", `"2w" is not a range; use 1h, 6h, 24h, 7d or 30d.`},
		{[]string{"export", "box-1", "--hub", "ftp://x"}, 1, "", `is not a hub address`},
		{[]string{"export", "box-1", "--range", "7d", "--from", "2026-10-30"}, 1, "", "Use either --range or --from/--to, not both."},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		code := run(tt.args, &out, &errOut)
		if code != tt.code || !strings.Contains(out.String(), tt.out) || !strings.Contains(errOut.String(), tt.errOut) {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d, %q, %q",
				tt.args, code, out.String(), errOut.String(), tt.code, tt.out, tt.errOut)
		}
		if code != 0 && out.Len() > 0 {
			t.Errorf("run(%q) failed but wrote %q to stdout", tt.args, out.String())
		}
	}
}

// fakeExportHub serves two devices and one minute of history for "id-1", answering only to key.
func fakeExportHub(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info" {
			fmt.Fprintf(w, `{"app":"device-monitor","proof":%q}`, agent.Proof(key, r.URL.Query().Get("nonce")))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/devices":
			io.WriteString(w, `[{"id":"id-1","name":"Upagentbox"},{"id":"id-2","name":"twin"},{"id":"id-3","name":"Twin"}]`)
		case strings.HasPrefix(r.URL.Path, "/api/history/id-1"):
			t := time.Date(2026, 10, 31, 11, 30, 0, 0, time.UTC).UnixMilli()
			fmt.Fprintf(w, `{"points":[{"t":%d,"cpu":42,"n":4}]}`, t)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// export --hub finds the key connect saved in client.json ("hubUrl"), takes a device name
// as well as an id, and honors the global --hub (skeptic M1 and the report's export gaps).
func TestExportFromHubUsesPairingAndNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	exportNow = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	defer func() { exportNow = time.Now }()
	srv := fakeExportHub(t, "0123456789abcdef")
	if err := agent.SaveClient(filepath.Join(dir, "client.json"), agent.Client{HubURL: srv.URL, Key: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"export", "id-1", "--range", "6h", "--hub", srv.URL},
		{"export", "upagentbox", "--range", "6h", "--hub", srv.URL},
		{"--hub", srv.URL, "export", "Upagentbox", "--range", "6h"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 || !strings.Contains(out.String(), ",42,") {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q", args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"export", "twin", "--hub", srv.URL}, &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "More than one device is named") {
		t.Errorf("ambiguous name: exit %d, %q", code, errOut.String())
	}
	// --key stands in for a missing pairing.
	if err := os.Remove(filepath.Join(dir, "client.json")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"export", "id-1", "--hub", srv.URL, "--key", "0123456789abcdef"}, &out, &errOut); code != 0 {
		t.Errorf("--key: exit %d, %q", code, errOut.String())
	}
	if code := run([]string{"export", "id-1", "--hub", srv.URL}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "locked") {
		t.Errorf("no key: exit %d, %q", code, errOut.String())
	}
}

// A name in this machine's own devices.json resolves to its id.
func TestExportLocalByName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	paths := config.At(dir)
	os.MkdirAll(paths.History, 0o700)
	b, _ := os.ReadFile(filepath.Join("..", "..", "internal", "export", "testdata", "history", "box-1.ndjson"))
	os.WriteFile(filepath.Join(paths.History, "box-1.ndjson"), b, 0o600)
	os.WriteFile(paths.Devices, []byte(`{"box-1":{"id":"box-1","name":"Garage PC"}}`), 0o600)
	exportNow = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	defer func() { exportNow = time.Now }()
	var out, errOut bytes.Buffer
	if code := run([]string{"export", "garage pc", "--range", "7d"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), ",80,97.5,") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}
