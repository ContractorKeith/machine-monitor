package hub

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/update"
	"github.com/ContractorKeith/machine-monitor/internal/update/updatetest"
)

// fakeBinary stands in for machine-monitor: it records each call's arguments.
const fakeBinary = "#!/bin/sh\necho \"$*\" >> \"$HOME/calls.log\"\n"

// withRelease points the installers at a fake release of version.
func withRelease(t *testing.T, version string) *updatetest.Release {
	t.Helper()
	bins := map[string][]byte{}
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"} {
		bins[p] = []byte(fakeBinary)
	}
	rel := updatetest.New(t, "v"+version, bins)
	oldV, oldS := InstallVersion, InstallSource
	InstallVersion, InstallSource = version, rel.Source
	t.Cleanup(func() { InstallVersion, InstallSource = oldV, oldS })
	return rel
}

func TestInstallNeedsKey(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	for _, p := range []string{"/install.sh", "/install.ps1"} {
		if w := th.req("GET", p, "", ""); w.Code != 401 {
			t.Errorf("%s without key: %d", p, w.Code)
		}
		if w := th.req("GET", p, "wrong", ""); w.Code != 401 {
			t.Errorf("%s with a wrong key: %d", p, w.Code)
		}
	}
}

func TestInstallScriptsPinVerifiedChecksums(t *testing.T) {
	rel := withRelease(t, "1.2.3")
	th := newTestHub(t)
	sums, err := update.ParseChecksums(rel.File("checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sh := th.keyed("GET", "/install.sh", "")
	ps := th.keyed("GET", "/install.ps1", "")
	for _, w := range []*httptest.ResponseRecorder{sh, ps} {
		if w.Code != 200 {
			t.Fatalf("installer: %d %s", w.Code, w.Body)
		}
		body := w.Body.String()
		if strings.Contains(body, "__") {
			t.Errorf("unfilled placeholder in\n%s", body)
		}
		for _, want := range []string{"'http://127.0.0.1:7777'", "'" + th.Token() + "'", "'1.2.3'",
			"'" + rel.Server.URL + "/ContractorKeith/machine-monitor/releases/download/v1.2.3'"} {
			if !strings.Contains(body, want) {
				t.Errorf("installer lacks %s", want)
			}
		}
	}
	for name, sum := range sums {
		inSh, inPs := strings.Contains(sh.Body.String(), sum), strings.Contains(ps.Body.String(), sum)
		windows := strings.Contains(name, "_windows_")
		if inSh == windows || inPs != windows {
			t.Errorf("%s: in install.sh %v, in install.ps1 %v", name, inSh, inPs)
		}
	}
	// checksums.txt is fetched and verified once, then cached.
	th.keyed("GET", "/install.sh", "")
	n := 0
	for _, p := range rel.Fetched() {
		if strings.HasSuffix(p, "/checksums.txt") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("checksums.txt fetched %d times, want 1", n)
	}
}

func TestInstallRefusesUnverifiedRelease(t *testing.T) {
	rel := withRelease(t, "1.2.4")
	key, cert := updatetest.NewCA(t)
	rel.Sign(update.WorkflowIdentity(update.Repo)("v1.2.4"), key, cert)
	th := newTestHub(t)
	w := th.keyed("GET", "/install.sh", "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "exit 1") {
		t.Fatalf("bad signature: %d %s", w.Code, w.Body)
	}
}

func TestInstallFromDevBuild(t *testing.T) {
	withRelease(t, "1.2.3")
	InstallVersion = "dev"
	th := newTestHub(t)
	w := th.keyed("GET", "/install.ps1", "")
	if w.Code != 503 || !strings.HasPrefix(w.Body.String(), "throw '") {
		t.Fatalf("dev build: %d %s", w.Code, w.Body)
	}
}

func TestInstallHubURL(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	w := th.req("GET", "/install.sh", th.Token(), "", "Host", "testbox.local:7777", "X-Forwarded-Proto", "https")
	if !strings.Contains(w.Body.String(), "HUB='https://testbox.local:7777'") {
		t.Fatalf("hub URL not taken from Host and X-Forwarded-Proto:\n%s", w.Body)
	}
	if w := th.req("GET", "/install.sh", th.Token(), "", "Host", "[::1]'x"); w.Code == 200 {
		t.Fatalf("odd Host accepted: %d", w.Code)
	}
}

// A value with quotes in it can't end the string it is pasted into.
func TestFillTemplateQuoting(t *testing.T) {
	evil := `a'b"c$(touch pwned)` + "`x`" + `\'; exit 3; '`
	if _, err := exec.LookPath("bash"); err == nil && runtime.GOOS != "windows" {
		script := fillTemplate(`X='__X__'; printf %s "$X"`, map[string]string{"X": evil}, shQuote)
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil || string(out) != evil {
			t.Fatalf("sh: got %q, %v; want %q", out, err, evil)
		}
	}
	if got := psQuote("it's ’x"); got != "it''s ’’x" {
		t.Fatalf("psQuote = %q", got)
	}
}

// installRun is the documented one-liner, curl ... | bash, against a live hub, in a temp HOME.
func installRun(t *testing.T, th *testHub) (home, hubURL string, out []byte, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for Linux and macOS")
	}
	for _, tool := range []string{"bash", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	srv := httptest.NewServer(th.h)
	t.Cleanup(srv.Close)
	home = t.TempDir()
	tmp := filepath.Join(home, "tmp")
	os.Mkdir(tmp, 0o700)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", `set -o pipefail; curl -fsSL -H "Authorization: Bearer $KEY" "$URL/install.sh" | bash`)
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + os.Getenv("PATH"),
		"KEY=" + th.Token(), "URL=" + srv.URL}
	out, err = cmd.CombinedOutput()
	if left, _ := os.ReadDir(tmp); len(left) > 0 {
		t.Errorf("installer left %d temp entries", len(left))
	}
	return home, srv.URL, out, err
}

func TestInstallShEndToEnd(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	home, hub, out, err := installRun(t, th)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	bin := filepath.Join(home, ".local", "bin", "machine-monitor")
	b, err := os.ReadFile(bin)
	if err != nil || string(b) != fakeBinary {
		t.Fatalf("installed binary %q, %v", b, err)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary not executable: %v", fi.Mode())
	}
	calls, _ := os.ReadFile(filepath.Join(home, "calls.log"))
	want := "connect --no-agent " + hub + "/?key=" + th.Token() + "\n" +
		"service install --role agent --hub " + hub + "\n"
	if string(calls) != want {
		t.Fatalf("binary called with\n%s\nwant\n%s\ninstaller output:\n%s", calls, want, out)
	}

	// Running it again replaces the binary in place.
	if _, _, out, err := installRun(t, th); err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
}

func TestInstallShRefusesBadChecksum(t *testing.T) {
	rel := withRelease(t, "1.2.3")
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		goos, goarch, _ := strings.Cut(p, "/")
		name := update.ArchiveName("1.2.3", goos, goarch)
		rel.Set(name, updatetest.Archive(t, name, []byte("#!/bin/sh\ntouch \"$HOME/pwned\"\n")))
	}
	th := newTestHub(t)
	home, _, out, err := installRun(t, th)
	if err == nil || !strings.Contains(string(out), "SHA-256") {
		t.Fatalf("tampered archive: err %v\n%s", err, out)
	}
	for _, f := range []string{".local/bin/machine-monitor", "calls.log", "pwned"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s exists after a failed install", f)
		}
	}
}

func TestInstallShShellCheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("no shellcheck")
	}
	if out, err := exec.Command(sc, "-s", "sh", "install.sh").CombinedOutput(); err != nil {
		t.Fatalf("shellcheck install.sh:\n%s", out)
	}
}

// install.ps1 parses in whatever PowerShell is here: Windows PowerShell 5.1 on Windows runners.
func TestInstallPs1Parses(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		if ps, err = exec.LookPath("pwsh"); err != nil {
			t.Skip("no PowerShell")
		}
	}
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	w := th.keyed("GET", "/install.ps1", "")
	path := filepath.Join(t.TempDir(), "install.ps1")
	if err := os.WriteFile(path, w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	check := `$e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile('` + psQuote(path) + `', [ref]$null, [ref]$e); ` +
		`if ($e.Count) { $e | ForEach-Object { $_.ToString() }; exit 1 }; $PSVersionTable.PSVersion.ToString()`
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", check).CombinedOutput()
	if err != nil {
		t.Fatalf("install.ps1 does not parse:\n%s", out)
	}
	t.Logf("parsed with PowerShell %s", bytes.TrimSpace(out))
}

// install.ps1, run for real by PowerShell 7 on Linux or macOS against a live hub, up to
// launching the binary (a .exe unpacked from a zip can't run there). Not run on Windows,
// where it would install a real agent and put a directory on the user's PATH.
func TestInstallPs1DownloadsAndVerifies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("would install a real agent")
	}
	ps, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh")
	}
	rel := withRelease(t, "1.2.3")
	th := newTestHub(t)
	srv := httptest.NewServer(th.h)
	t.Cleanup(srv.Close)
	run := func() (exe string, out []byte) {
		home := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command",
			`irm -Headers @{Authorization="Bearer $env:KEY"} "$env:URL/install.ps1" | iex`)
		cmd.Env = []string{"HOME=" + home, "LOCALAPPDATA=" + home, "TMPDIR=" + home, "PATH=" + os.Getenv("PATH"),
			"PROCESSOR_ARCHITECTURE=AMD64", "KEY=" + th.Token(), "URL=" + srv.URL}
		out, _ = cmd.CombinedOutput()
		return filepath.Join(home, "Programs", "machine-monitor", "machine-monitor.exe"), out
	}
	exe, out := run()
	if b, err := os.ReadFile(exe); err != nil || string(b) != fakeBinary {
		t.Fatalf("installed %q, %v\n%s", b, err, out)
	}

	name := update.ArchiveName("1.2.3", "windows", "amd64")
	rel.Set(name, updatetest.Archive(t, name, []byte("evil")))
	exe, out = run()
	if !strings.Contains(string(out), "SHA-256") {
		t.Fatalf("tampered archive not refused:\n%s", out)
	}
	if _, err := os.Stat(exe); err == nil {
		t.Fatal("binary installed after a failed check")
	}
}
