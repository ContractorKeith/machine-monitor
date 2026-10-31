package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/charmbracelet/x/exp/golden"
)

// now is the fixture clock: testdata/history holds minutes from 2026-10-20 and 2026-10-31.
var now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return now }

// fixture copies testdata/history to a temp dir, since opening a store may rewrite files.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir("testdata/history")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata/history", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func span(t *testing.T, rng string) (time.Time, time.Time) {
	t.Helper()
	from, to, err := ParseRange(rng, "", "", now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return from, to
}

func export(t *testing.T, src Source, id, rng, format string) (string, error) {
	t.Helper()
	from, to := span(t, rng)
	var buf bytes.Buffer
	err := Run(context.Background(), src, Options{ID: id, From: from, To: to, Format: format}, &buf)
	return buf.String(), err
}

func TestGolden(t *testing.T) {
	src := Local{Dir: fixture(t), Now: clock}
	for _, format := range []string{"csv", "json"} {
		t.Run(format, func(t *testing.T) {
			out, err := export(t, src, "box-1", "30d", format)
			if err != nil {
				t.Fatal(err)
			}
			golden.RequireEqual(t, out)
		})
	}
}

func TestRangeFilters(t *testing.T) {
	src := Local{Dir: fixture(t), Now: clock}
	for rng, rows := range map[string]int{"30d": 4, "7d": 3, "6h": 3, "24h": 3} {
		out, err := export(t, src, "box-1", rng, "csv")
		if err != nil {
			t.Fatalf("%s: %v", rng, err)
		}
		if got := strings.Count(out, "\n") - 1; got != rows {
			t.Errorf("%s: %d rows, want %d", rng, got, rows)
		}
	}
}

func TestParseRange(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	tests := []struct {
		rng, from, to string
		loc           *time.Location
		wantFrom      time.Time
		wantTo        time.Time
	}{
		{"", "", "", time.UTC, now.Add(-24 * time.Hour), now},
		{"1h", "", "", time.UTC, now.Add(-time.Hour), now},
		{"6h", "", "", time.UTC, now.Add(-6 * time.Hour), now},
		{"24h", "", "", time.UTC, now.Add(-24 * time.Hour), now},
		{"7d", "", "", time.UTC, now.AddDate(0, 0, -7), now},
		{"30D", "", "", time.UTC, now.AddDate(0, 0, -30), now},
		{"", "", "2026-10-30T10:00:00Z", time.UTC, time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)},
		{"", "2026-10-01T00:00:00+02:00", "", time.UTC, time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC), now},
		{"", "2026-10-01", "2026-10-02", ny, time.Date(2026, 10, 1, 0, 0, 0, 0, ny), time.Date(2026, 10, 2, 0, 0, 0, 0, ny)},
	}
	for _, tt := range tests {
		from, to, err := ParseRange(tt.rng, tt.from, tt.to, now, tt.loc)
		if err != nil || !from.Equal(tt.wantFrom) || !to.Equal(tt.wantTo) {
			t.Errorf("ParseRange(%q, %q, %q) = %v, %v, %v; want %v, %v", tt.rng, tt.from, tt.to, from, to, err, tt.wantFrom, tt.wantTo)
		}
	}

	bad := []struct{ rng, from, to, want string }{
		{"2w", "", "", `"2w" is not a range; use 1h, 6h, 24h, 7d or 30d.`},
		{"", "yesterday", "", `--from "yesterday" is not a time; use RFC 3339`},
		{"", "", "10/31/2026", `--to "10/31/2026" is not a time`},
		{"7d", "2026-10-01", "", "Use either --range or --from/--to, not both."},
		{"1h", "", "2026-10-30T10:00:00Z", "Use either --range or --from/--to, not both."},
		{"", "2026-10-31T13:00:00Z", "", "The start of the range (2026-10-31T13:00:00Z) must be before its end (2026-10-31T12:00:00Z)."},
	}
	for _, tt := range bad {
		_, _, err := ParseRange(tt.rng, tt.from, tt.to, now, time.UTC)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) || !strings.HasSuffix(err.Error(), ".") {
			t.Errorf("ParseRange(%q, %q, %q) error %v; want a sentence starting %q", tt.rng, tt.from, tt.to, err, tt.want)
		}
	}
}

func TestErrors(t *testing.T) {
	src := Local{Dir: fixture(t), Now: clock}
	missing := Local{Dir: filepath.Join(t.TempDir(), "history"), Now: clock}
	tests := []struct {
		src             Source
		id, rng, format string
		want            string
	}{
		{src, "ghost", "7d", "csv", `There is no device "ghost" in this machine's history.`},
		{missing, "box-1", "7d", "csv", `There is no device "box-1" in this machine's history.`},
		{src, "box-1", "1h", "csv", `Device "box-1" has no history between 2026-10-31T11:00:00Z and 2026-10-31T12:00:00Z.`},
		{src, "nas", "1h", "json", `Device "nas" has no history between`},
		{src, "box-1", "7d", "xml", `"xml" is not a format; use csv or json.`},
		{src, "../etc", "7d", "csv", `"../etc" is not a device ID;`},
	}
	for _, tt := range tests {
		out, err := export(t, tt.src, tt.id, tt.rng, tt.format)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) || out != "" {
			t.Errorf("export %s %s %s: err %v, output %q; want error starting %q and no output", tt.id, tt.rng, tt.format, err, out, tt.want)
		}
	}
	if _, err := os.Stat(missing.Dir); !os.IsNotExist(err) {
		t.Errorf("export created the missing history directory (stat err %v)", err)
	}
}

// fakeHub serves GET /api/history/{id} from a store the way the hub does: key check, 404 for
// a device it doesn't know, then Params and Query.
func fakeHub(t *testing.T, dir, key string, requests *int) *httptest.Server {
	t.Helper()
	st, err := history.Open(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/history/{id}", func(w http.ResponseWriter, r *http.Request) {
		*requests++
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"This hub is locked. Connect with the hub's key."}`)
			return
		}
		id := r.PathValue("id")
		if len(st.MinutePoints(id, 0, 1e300)) == 0 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found"}`)
			return
		}
		from, to, points, err := st.Params(r.URL.Query())
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"bad range"}`)
			return
		}
		b, _ := json.Marshal(st.Query(id, from, to, points))
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestHubMatchesLocal checks a hub export is byte-for-byte the local one.
func TestHubMatchesLocal(t *testing.T) {
	dir := fixture(t)
	var n int
	srv := fakeHub(t, dir, "k3y", &n)
	hub := Hub{URL: srv.URL, Key: "k3y"}
	for _, format := range []string{"csv", "json"} {
		want, err := export(t, Local{Dir: dir, Now: clock}, "box-1", "30d", format)
		if err != nil {
			t.Fatal(err)
		}
		got, err := export(t, hub, "box-1", "30d", format)
		if err != nil || got != want {
			t.Errorf("%s: hub export = %q, %v; want %q", format, got, err, want)
		}
	}
}

// TestHubWindows checks a range longer than 3000 minutes comes back whole, at full minute
// resolution, across several requests.
func TestHubWindows(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	start := now.Add(-5 * 24 * time.Hour)
	minutes := 0
	for ts := start; ts.Before(now); ts = ts.Add(time.Minute) {
		fmt.Fprintf(&buf, `{"id":"big","t":%d,"cpu":%d,"n":30}`+"\n", ts.UnixMilli(), minutes%100)
		minutes++
	}
	if err := os.WriteFile(filepath.Join(dir, "big.ndjson"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var n int
	srv := fakeHub(t, dir, "k", &n)
	from, to := span(t, "7d")
	got, err := Hub{URL: srv.URL, Key: "k"}.Points(context.Background(), "big", from, to)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Local{Dir: dir, Now: clock}.Points(context.Background(), "big", from, to)
	if len(got) != minutes || !reflect.DeepEqual(got, want) {
		t.Errorf("hub returned %d points, want %d identical to local", len(got), minutes)
	}
	if n != 4 { // 7 days = 10080 minutes = 4 windows of up to 3000
		t.Errorf("%d requests, want 4", n)
	}
}

func TestHubErrors(t *testing.T) {
	var n int
	srv := fakeHub(t, fixture(t), "right", &n)
	tests := []struct {
		hub  Hub
		id   string
		want string
	}{
		{Hub{URL: srv.URL, Key: "wrong"}, "box-1", "The hub at " + srv.URL + " is locked and this machine has no key for it. Pair with it first: machine-monitor connect " + srv.URL},
		{Hub{URL: srv.URL, Key: "right"}, "ghost", `There is no device "ghost" on the hub at ` + srv.URL + "."},
		{Hub{URL: "http://127.0.0.1:1", Key: "right"}, "box-1", "Could not reach the hub at http://127.0.0.1:1:"},
	}
	for _, tt := range tests {
		_, err := export(t, tt.hub, tt.id, "7d", "csv")
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%s %s: err %v; want %q", tt.hub.URL, tt.id, err, tt.want)
		}
	}
}

// TestHubTooLarge checks a reply cut at maxBody is reported as too large, not as bad JSON.
func TestHubTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"points":[`))
		w.Write(bytes.Repeat([]byte(" "), maxBody))
	}))
	defer srv.Close()
	_, err := export(t, Hub{URL: srv.URL}, "box-1", "1h", "csv")
	want := "The hub at " + srv.URL + " sent a reply larger than 32 MB"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("err %v; want %q", err, want)
	}
}

func TestHubURL(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.10:7777":      "http://192.168.1.10:7777",
		"http://Box.local:7777/": "http://box.local:7777",
		"https://hub.ts.net/mm/": "https://hub.ts.net/mm",
		" http://10.0.0.2:7777 ": "http://10.0.0.2:7777",
	} {
		if got, err := HubURL(in); err != nil || got != want {
			t.Errorf("HubURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ftp://x", "http://", ""} {
		if _, err := HubURL(in); err == nil {
			t.Errorf("HubURL(%q) succeeded, want error", in)
		}
	}
}

func TestKeyFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	if k, err := KeyFor(path, "http://a:7777"); k != "" || err != nil {
		t.Errorf("missing file: %q, %v", k, err)
	}
	os.WriteFile(path, []byte(`{"hub":"http://A:7777/","key":"s3cret"}`), 0o600)
	if k, err := KeyFor(path, "http://a:7777"); k != "s3cret" || err != nil {
		t.Errorf("same hub: %q, %v; want the key", k, err)
	}
	// connect writes "hubUrl" (agent.SaveClient), upstream writes "token".
	os.WriteFile(path, []byte(`{"hubUrl":"http://a:7777","token":"fromconnect"}`), 0o600)
	if k, err := KeyFor(path, "http://a:7777"); k != "fromconnect" || err != nil {
		t.Errorf("hubUrl file: %q, %v; want the key", k, err)
	}
	if k, err := KeyFor(path, "http://b:7777"); k != "" || err != nil {
		t.Errorf("other hub: %q, %v; want no key", k, err)
	}
	os.WriteFile(path, []byte(`{`), 0o600)
	if _, err := KeyFor(path, "http://a:7777"); err == nil || !strings.HasSuffix(err.Error(), ".") {
		t.Errorf("bad JSON: err %v, want a sentence", err)
	}
}
