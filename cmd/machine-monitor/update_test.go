package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/update"
	"github.com/ContractorKeith/machine-monitor/internal/update/updatetest"
)

func init() { built["update"] = true }

// fakeUpdate serves release tag with a build for this platform and points update at a
// fake binary in a temp dir. It returns the release and the binary's path.
func fakeUpdate(t *testing.T, tag string) (*updatetest.Release, string) {
	t.Helper()
	rel := updatetest.New(t, tag, map[string][]byte{runtime.GOOS + "/" + runtime.GOARCH: []byte("new binary")})
	exe := filepath.Join(t.TempDir(), "machine-monitor")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldS, oldE, oldV := updateSource, updateExecutable, version
	updateSource = func() update.Source { return rel.Source }
	updateExecutable = func() (string, error) { return exe, nil }
	version = "1.0.0"
	t.Cleanup(func() { updateSource, updateExecutable, version = oldS, oldE, oldV })
	return rel, exe
}

func runUpdateCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"update"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUpdateInstallsLatest(t *testing.T) {
	_, exe := fakeUpdate(t, "v1.1.0")
	code, out, errOut := runUpdateCmd(t)
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, errOut)
	}
	for _, want := range []string{"This is machine-monitor 1.0.0. The latest release is 1.1.0 (2026-10-30).",
		"What's new in 1.1.0:", "Faster charts", "Signature and checksum verified.", "Updated " + exe + " to 1.1.0."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("binary is %q after update", b)
	}
}

func TestUpdateCheckOnly(t *testing.T) {
	rel, exe := fakeUpdate(t, "v1.1.0")
	code, out, errOut := runUpdateCmd(t, "--check")
	if code != 0 || !strings.Contains(out, "Run `machine-monitor update` to install it.") {
		t.Fatalf("--check: %d\n%s%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("--check changed the binary")
	}
	for _, p := range rel.Fetched() {
		if strings.Contains(p, "/releases/download/") {
			t.Errorf("--check downloaded %s", p)
		}
	}
}

func TestUpdateUpToDate(t *testing.T) {
	_, exe := fakeUpdate(t, "v1.0.0")
	code, out, _ := runUpdateCmd(t)
	if code != 0 || !strings.Contains(out, "You're up to date.") {
		t.Fatalf("up to date: %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("binary changed")
	}
}

func TestUpdateRefusesBadChecksum(t *testing.T) {
	rel, exe := fakeUpdate(t, "v1.1.0")
	name := update.ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)
	rel.Set(name, updatetest.Archive(t, name, []byte("evil binary")))
	code, _, errOut := runUpdateCmd(t)
	if code != 1 || !strings.Contains(errOut, "SHA-256 mismatch") || !strings.Contains(errOut, "nothing was changed") {
		t.Fatalf("bad checksum: %d %s", code, errOut)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("binary replaced despite a bad checksum")
	}
}

func TestUpdateRefusesBadSignature(t *testing.T) {
	rel, exe := fakeUpdate(t, "v1.1.0")
	// A matching checksums.txt for a tampered archive, signed by someone else's CA.
	name := update.ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)
	evil := updatetest.Archive(t, name, []byte("evil binary"))
	rel.Set(name, evil)
	rel.Set("checksums.txt", []byte(sha256Line(evil, name)))
	key, cert := updatetest.NewCA(t)
	rel.Sign(update.WorkflowIdentity(update.Repo)("v1.1.0"), key, cert)
	code, _, errOut := runUpdateCmd(t)
	if code != 1 || !strings.Contains(errOut, "signature") {
		t.Fatalf("bad signature: %d %s", code, errOut)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("binary replaced despite a bad signature")
	}
}

func TestUpdateLeavesPackageManagersAlone(t *testing.T) {
	fakeUpdate(t, "v1.1.0")
	updateExecutable = func() (string, error) {
		return "/opt/homebrew/Caskroom/machine-monitor/1.0.0/machine-monitor", nil
	}
	code, _, errOut := runUpdateCmd(t)
	if code != 1 || !strings.Contains(errOut, "brew upgrade machine-monitor") {
		t.Fatalf("Homebrew binary: %d %s", code, errOut)
	}
}

func TestPrintableStripsEscapes(t *testing.T) {
	if got := printable("a\x1b[31mred\x07\u009b"); got != "a[31mred" {
		t.Fatalf("printable = %q", got)
	}
}

func sha256Line(data []byte, name string) string {
	var b bytes.Buffer
	b.WriteString(sha256Hex(data))
	b.WriteString("  " + name + "\n")
	return b.String()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Before the first release GitHub answers 404; update says so and exits 0 (skeptic L8).
func TestUpdateWithNoReleaseYet(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	oldS, oldV := updateSource, version
	updateSource = func() update.Source { return update.Source{API: srv.URL, Download: srv.URL, Repo: update.Repo} }
	version = "dev"
	t.Cleanup(func() { updateSource, version = oldS, oldV })
	code, out, errOut := runUpdateCmd(t, "--check")
	if code != 0 || errOut != "" || !strings.Contains(out, "machine-monitor dev. No release has been published yet") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
