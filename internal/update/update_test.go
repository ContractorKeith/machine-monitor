package update_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/update"
	"github.com/ContractorKeith/machine-monitor/internal/update/updatetest"
)

var allPlatforms = map[string][]byte{
	"linux/amd64": []byte("linux-amd64-bin"), "linux/arm64": []byte("linux-arm64-bin"),
	"darwin/amd64": []byte("darwin-amd64-bin"), "darwin/arm64": []byte("darwin-arm64-bin"),
	"windows/amd64": []byte("windows-amd64-bin"), "windows/arm64": []byte("windows-arm64-bin"),
}

func TestLatestAndBinary(t *testing.T) {
	rel := updatetest.New(t, "v1.4.0", allPlatforms)
	ctx := context.Background()
	got, err := rel.Source.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v1.4.0" || got.Version() != "1.4.0" || !strings.Contains(got.Notes, "Faster charts") {
		t.Fatalf("Latest = %+v", got)
	}
	for plat, want := range allPlatforms {
		goos, goarch, _ := strings.Cut(plat, "/")
		bin, err := rel.Source.Binary(ctx, got.Tag, goos, goarch)
		if err != nil {
			t.Fatalf("%s: %v", plat, err)
		}
		if !bytes.Equal(bin, want) {
			t.Fatalf("%s: binary %q, want %q", plat, bin, want)
		}
	}
}

func TestBinaryRefusesBadChecksum(t *testing.T) {
	rel := updatetest.New(t, "v1.4.0", allPlatforms)
	name := update.ArchiveName("1.4.0", "linux", "amd64")
	rel.Set(name, updatetest.Archive(t, name, []byte("evil")))
	_, err := rel.Source.Binary(context.Background(), "v1.4.0", "linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v, want a SHA-256 mismatch", err)
	}
}

func TestChecksumsRefusesBadSignature(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*updatetest.Release){
		"checksums edited after signing": func(r *updatetest.Release) {
			r.Set("checksums.txt", append(r.File("checksums.txt"), "0000000000000000000000000000000000000000000000000000000000000000  extra\n"...))
		},
		"untrusted CA": func(r *updatetest.Release) {
			key, cert := updatetest.NewCA(t)
			r.Sign(update.WorkflowIdentity(update.Repo)("v1.4.0"), key, cert)
		},
		"other repo's workflow": func(r *updatetest.Release) {
			key, cert := r.CA()
			r.Sign(update.WorkflowIdentity("someone/fork")("v1.4.0"), key, cert)
		},
		"signed for another tag": func(r *updatetest.Release) {
			key, cert := r.CA()
			r.Sign(update.WorkflowIdentity(update.Repo)("v1.3.0"), key, cert)
		},
		"signed through another OIDC issuer": func(r *updatetest.Release) {
			key, cert := r.CA()
			r.SignIssuer(update.WorkflowIdentity(update.Repo)("v1.4.0"), "https://accounts.example.com", key, cert)
		},
		"no bundle":      func(r *updatetest.Release) { r.Set("checksums.txt.sigstore.json", []byte("{}")) },
		"garbage bundle": func(r *updatetest.Release) { r.Set("checksums.txt.sigstore.json", []byte("not json")) },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			rel := updatetest.New(t, "v1.4.0", allPlatforms)
			tamper(rel)
			if _, err := rel.Source.Checksums(ctx, "v1.4.0"); err == nil {
				t.Fatal("Checksums accepted a bad signature")
			}
			if _, err := rel.Source.Binary(ctx, "v1.4.0", "linux", "amd64"); err == nil {
				t.Fatal("Binary accepted a bad signature")
			}
		})
	}
}

// The realistic attack: Fulcio certificates are public, so an attacker replays a genuine
// certificate and signature, edits checksums.txt and drops the bundle's messageDigest. Only
// the signature check itself can refuse that.
func TestChecksumsRefusesEditedChecksumsWithoutDigest(t *testing.T) {
	rel := updatetest.New(t, "v1.4.0", allPlatforms)
	var b map[string]any
	if err := json.Unmarshal(rel.File("checksums.txt.sigstore.json"), &b); err != nil {
		t.Fatal(err)
	}
	delete(b["messageSignature"].(map[string]any), "messageDigest")
	bundle, _ := json.Marshal(b)
	rel.Set("checksums.txt.sigstore.json", bundle)
	if _, err := rel.Source.Checksums(context.Background(), "v1.4.0"); err != nil {
		t.Fatalf("untouched checksums.txt without digest: %v", err)
	}
	rel.Set("checksums.txt", append(rel.File("checksums.txt"), "0000000000000000000000000000000000000000000000000000000000000000  extra\n"...))
	_, err := rel.Source.Checksums(context.Background(), "v1.4.0")
	if err == nil || !strings.Contains(err.Error(), "signature does not match") {
		t.Fatalf("err = %v, want signature does not match", err)
	}
}

func TestDefaultTrustLoadsFulcio(t *testing.T) {
	tr := update.DefaultTrust(update.Repo)
	if tr.Roots == nil || tr.Intermediates == nil || tr.Issuer != update.GitHubIssuer {
		t.Fatalf("DefaultTrust = %+v", tr)
	}
	want := "https://github.com/ContractorKeith/machine-monitor/.github/workflows/release.yml@refs/tags/v1.0.0"
	if got := tr.Identity("v1.0.0"); got != want {
		t.Fatalf("identity %q, want %q", got, want)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.4", "1.2.3", true},
		{"v1.10.0", "1.9.9", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.3", "1.2.3", false},
		{"v1.2.3", "1.2.4", false},
		{"v1.2.3", "dev", true},
		{"v1.2.3", "0.0.0-SNAPSHOT-68b9fc1", true},
		{"garbage", "1.0.0", false},
	}
	for _, c := range cases {
		if got := update.Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	m, err := update.ParseChecksums([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  a.tar.gz\r\n\n"))
	if err != nil || m["a.tar.gz"] != strings.Repeat("a", 64) {
		t.Fatalf("ParseChecksums = %v, %v", m, err)
	}
	for _, bad := range []string{"", "xyz  a.tar.gz\n", strings.Repeat("a", 64) + "  ../evil\n"} {
		if _, err := update.ParseChecksums([]byte(bad)); err == nil {
			t.Errorf("ParseChecksums(%q) accepted", bad)
		}
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "machine-monitor")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := update.Replace(exe, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("exe = %q", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
			t.Fatalf("mode %v is not executable", fi.Mode())
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".machine-monitor-new-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestManagedBy(t *testing.T) {
	cases := map[string]string{
		"/opt/homebrew/Caskroom/machine-monitor/1.0.0/machine-monitor":      "Homebrew",
		"/usr/local/Cellar/machine-monitor/1.0.0/bin/machine-monitor":       "Homebrew",
		`C:\Users\k\scoop\apps\machine-monitor\current\machine-monitor.exe`: "Scoop",
		"/home/k/.local/bin/machine-monitor":                                "",
	}
	for p, want := range cases {
		if runtime.GOOS != "windows" && strings.Contains(p, `\`) {
			p = strings.ReplaceAll(p, `\`, "/")
		}
		if got := update.ManagedBy(p); got != want {
			t.Errorf("ManagedBy(%q) = %q, want %q", p, got, want)
		}
	}
}

// A real cosign keyless bundle: goreleaser's own v2.19.0 release, signed by its release.yml
// through the public Fulcio CA, checks out against the embedded root.
func TestVerifyRealSigstoreBundle(t *testing.T) {
	dir := filepath.Join("testdata", "goreleaser-v2.19.0")
	sums, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join(dir, "checksums.txt.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	tr := update.DefaultTrust("goreleaser/goreleaser")
	if err := tr.Verify(sums, bundle, "v2.19.0"); err != nil {
		t.Fatalf("real bundle: %v", err)
	}
	if err := tr.Verify(append(sums, '\n'), bundle, "v2.19.0"); err == nil {
		t.Fatal("accepted edited checksums")
	}
	if err := update.DefaultTrust(update.Repo).Verify(sums, bundle, "v2.19.0"); err == nil {
		t.Fatal("accepted another repo's signature")
	}
}

func TestRateLimitMessage(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	src := update.Source{API: srv.URL, Download: srv.URL, Repo: update.Repo, Client: srv.Client()}
	_, err := src.Latest(context.Background())
	want := "GitHub API rate limit reached; it resets at " + reset.Format("15:04 MST")
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
