package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// connect also records what it read on stdin.
// FAKE_FAIL=<command> makes that command exit 3. A successful connect writes client.json.
const fakeBinary = "#!/bin/sh\necho \"$*\" >> \"$HOME/calls.log\"\n" +
	"if [ \"$1\" = connect ]; then cat >> \"$HOME/stdin.log\"; fi\n" +
	"if [ \"$1\" = \"${FAKE_FAIL:-}\" ]; then exit 3; fi\n" +
	"if [ \"$1\" = connect ]; then mkdir -p \"${MACHINE_MONITOR_DIR:-$HOME/cfg}\"; echo '{}' > \"${MACHINE_MONITOR_DIR:-$HOME/cfg}/client.json\"; fi\n"

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
	https := func(remote string) bool {
		w := th.req("GET", "/install.sh", th.Token(), "", "Host", "testbox.local:7777",
			"X-Forwarded-Proto", "https", "RemoteAddr", remote)
		if !strings.Contains(w.Body.String(), "://testbox.local:7777'") {
			t.Fatalf("hub URL not taken from Host:\n%s", w.Body)
		}
		return strings.Contains(w.Body.String(), "HUB='https://testbox.local:7777'")
	}
	if https("127.0.0.1:50000") {
		t.Error("X-Forwarded-Proto honoured with no trusted proxy configured")
	}
	th.mu.Lock()
	th.cfg.extra = map[string]json.RawMessage{"trustedProxies": json.RawMessage(`["127.0.0.1", "100.64.0.0/10"]`)}
	th.mu.Unlock()
	if !https("127.0.0.1:50000") || !https("100.100.1.2:50000") {
		t.Error("X-Forwarded-Proto ignored from a trusted proxy")
	}
	if https("10.0.0.5:50000") {
		t.Error("X-Forwarded-Proto honoured from an untrusted address")
	}
	if w := th.req("GET", "/install.sh", th.Token(), "", "Host", "[::1]'x"); w.Code == 200 {
		t.Fatalf("odd Host accepted: %d", w.Code)
	}
	// An IPv6 zone in URL form makes a working hub URL.
	w := th.req("GET", "/install.sh", th.Token(), "", "Host", "[fe80::1%25eth0]:7777")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "HUB='http://[fe80::1%25eth0]:7777'") {
		t.Fatalf("zoned IPv6 Host: %d\n%s", w.Code, w.Body)
	}
}

func TestInstallHost(t *testing.T) {
	for host, want := range map[string]bool{
		"hub.lan": true, "hub.lan:7777": true, "192.168.1.5:65535": true, "[::1]": true, "[::1]:7777": true,
		"[fe80::1%25eth0]:7777": true, "[fe80::1%25en0.5]": true,
		"[fe80::1%a]":          false, // not URL form: http://[fe80::1%a] is not a valid URL
		"[fe80::1%25]":         false,
		"[fe80::1%25eth0%25x]": false,
		"[fe80::1%25eth/0]":    false,
		"127.0.0.1:99999999":   false,
		"127.0.0.1:0":          false,
		"127.0.0.1:":           false,
		"127.0.0.1:07777":      false,
		"fe80::1":              false, // IPv6 needs brackets
		"[127.0.0.1]":          false,
		"[::1":                 false,
		"[::1]'x":              false,
		"hub lan":              false,
		"":                     false,
	} {
		if got := installHost(host); got != want {
			t.Errorf("installHost(%q) = %v, want %v", host, got, want)
		}
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

// The documented ways to run install.sh: the one-liner, and the download-first form for
// curl older than 7.76 (no --fail-with-body). Both use $KEY and $URL.
const (
	oneLiner      = `curl -sSL --fail-with-body -H "Authorization: Bearer $KEY" "$URL/install.sh" | sh`
	downloadFirst = `curl -fsSL -H "Authorization: Bearer $KEY" -o install.sh "$URL/install.sh" && sh install.sh`
)

// installRun runs the one-liner with the hub's key against a live hub, in a temp HOME.
func installRun(t *testing.T, th *testHub) (home, hubURL string, out []byte, err error) {
	return installRunWith(t, th, th.Token(), oneLiner)
}

// installRunWith runs command (oneLiner or downloadFirst) with key; extra is added to the
// environment.
func installRunWith(t *testing.T, th *testHub, key, command string, extra ...string) (home, hubURL string, out []byte, err error) {
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
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = home
	cmd.Env = append([]string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + os.Getenv("PATH"),
		"KEY=" + key, "URL=" + srv.URL}, extra...)
	out, err = cmd.CombinedOutput()
	if left, _ := os.ReadDir(tmp); len(left) > 0 {
		t.Errorf("installer left %d temp entries", len(left))
	}
	return home, srv.URL, out, err
}

// wantFailure checks that an install exited non-zero, said msg, and left no binary behind.
func wantFailure(t *testing.T, what, home string, out []byte, err error, msg string) {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Errorf("%s: want a non-zero exit, got %v\n%s", what, err, out)
	}
	if !strings.Contains(string(out), msg) {
		t.Errorf("%s: output lacks %q:\n%s", what, msg, out)
	}
	if _, err := os.Stat(filepath.Join(home, "pwned")); err == nil {
		t.Errorf("%s: a tampered binary ran", what)
	}
}

func TestInstallShEndToEnd(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	for _, form := range []string{oneLiner, downloadFirst} {
		home, hub, out, err := installRunWith(t, th, th.Token(), form)
		if err != nil {
			t.Fatalf("%s: installer failed: %v\n%s", form, err, out)
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
		want := "connect --no-agent --key-stdin " + hub + "\n" +
			"service install --role agent --hub " + hub + "\n"
		if string(calls) != want {
			t.Fatalf("binary called with\n%s\nwant\n%s\ninstaller output:\n%s", calls, want, out)
		}
		if in, _ := os.ReadFile(filepath.Join(home, "stdin.log")); string(in) != th.Token()+"\n" {
			t.Fatalf("connect read %q on stdin, want the key", in)
		}
		if strings.Contains(string(calls), th.Token()) {
			t.Fatal("the key is in the binary's arguments")
		}
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
	wantFailure(t, "tampered archive", home, out, err, "failed its SHA-256 check")
	for _, f := range []string{".local/bin/machine-monitor", "calls.log"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s exists after a failed install", f)
		}
	}
}

// A failed download inside the script (the archive 404s) stops it with a non-zero exit.
func TestInstallShFailsOnDownloadError(t *testing.T) {
	rel := withRelease(t, "1.2.3")
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		goos, goarch, _ := strings.Cut(p, "/")
		rel.Remove(update.ArchiveName("1.2.3", goos, goarch))
	}
	th := newTestHub(t)
	home, _, out, err := installRun(t, th)
	wantFailure(t, "archive 404", home, out, err, "download failed")
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "machine-monitor")); err == nil {
		t.Error("binary installed after a failed download")
	}
}

// A failing step after the download (here: pairing) stops the script before the service.
func TestInstallShFailsWhenPairingFails(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	home, _, out, err := installRunWith(t, th, th.Token(), oneLiner, "FAKE_FAIL=connect")
	wantFailure(t, "connect fails", home, out, err, "pairing with")
	if calls, _ := os.ReadFile(filepath.Join(home, "calls.log")); strings.Contains(string(calls), "service") {
		t.Error("service installed after pairing failed")
	}
}

// Both documented forms fail with a non-zero exit when the hub refuses. The one-liner also
// shows why: curl --fail-with-body hands sh the hub's error script.
func TestInstallShFailsLoudlyOnHubError(t *testing.T) {
	rel := withRelease(t, "1.2.5")
	key, cert := updatetest.NewCA(t)
	rel.Sign(update.WorkflowIdentity(update.Repo)("v1.2.5"), key, cert)
	th := newTestHub(t)

	home, _, out, err := installRun(t, th)
	wantFailure(t, "hub 502", home, out, err, "couldn't get verified checksums")
	home, _, out, err = installRunWith(t, th, "wrong", oneLiner)
	wantFailure(t, "wrong key", home, out, err, "the hub key is missing or wrong")
	home, _, out, err = installRunWith(t, th, th.Token(), downloadFirst)
	wantFailure(t, "hub 502, download first", home, out, err, "502")
	home, _, out, err = installRunWith(t, th, "wrong", downloadFirst)
	wantFailure(t, "wrong key, download first", home, out, err, "401")
	if _, err := os.Stat(filepath.Join(home, "calls.log")); err == nil {
		t.Error("ran the binary after a hub error")
	}
}

// A script cut off mid-download runs nothing: all of it is inside main, called last.
func TestInstallShTruncatedRunsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for Linux and macOS")
	}
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	full := th.keyed("GET", "/install.sh", "").Body.String()
	if !strings.HasSuffix(full, "\nmain \"$@\"\n") {
		t.Fatal("install.sh doesn't end by calling main")
	}
	for _, cut := range []int{len(full) / 3, len(full) / 2, len(full) - len("main \"$@\"\n")} {
		home := t.TempDir()
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(full[:cut])
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "Downloading") {
			t.Errorf("cut at %d of %d: the install started:\n%s", cut, len(full), out)
		}
		if entries, _ := os.ReadDir(home); len(entries) > 0 {
			t.Errorf("cut at %d: wrote %d entries in HOME", cut, len(entries))
		}
	}
}

func TestInstallShShellCheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err == nil {
		err = exec.Command(sc, "--version").Run() // a version-manager shim may be unconfigured
	}
	if err != nil {
		t.Skip("no working shellcheck")
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
	run := func(key string) (exe string, out []byte, err error) {
		home := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command",
			`irm -Headers @{Authorization="Bearer $env:KEY"} "$env:URL/install.ps1" | iex`)
		cmd.Env = []string{"HOME=" + home, "LOCALAPPDATA=" + home, "TMPDIR=" + home, "PATH=" + os.Getenv("PATH"),
			"PROCESSOR_ARCHITECTURE=AMD64", "KEY=" + key, "URL=" + srv.URL}
		out, err = cmd.CombinedOutput()
		return filepath.Join(home, "Programs", "machine-monitor", "machine-monitor.exe"), out, err
	}
	exe, out, _ := run(th.Token())
	if b, err := os.ReadFile(exe); err != nil || string(b) != fakeBinary {
		t.Fatalf("installed %q, %v\n%s", b, err, out)
	}

	name := update.ArchiveName("1.2.3", "windows", "amd64")
	rel.Set(name, updatetest.Archive(t, name, []byte("evil")))
	exe, out, err = run(th.Token())
	wantFailure(t, "tampered archive", t.TempDir(), out, err, "failed its SHA-256 check")
	if _, err := os.Stat(exe); err == nil {
		t.Fatal("binary installed after a failed check")
	}
	// irm throws on the hub's refusal, so iex never runs and PowerShell exits non-zero.
	exe, out, err = run("wrong")
	wantFailure(t, "wrong key", t.TempDir(), out, err, "the hub key is missing or wrong")
	if _, err := os.Stat(exe); err == nil {
		t.Fatal("binary installed with a wrong key")
	}
}

// cfgDir points the data dir inside the fake home on every OS.
const cfgDir = `export MACHINE_MONITOR_DIR="$HOME/cfg"; `

// A failure after the binary is installed rolls the install back: a fresh install leaves no
// binary or client.json behind, and a replaced binary is put back (skeptic pass 2).
func TestInstallShRollsBack(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	home, _, out, err := installRunWith(t, th, th.Token(), cfgDir+oneLiner, "FAKE_FAIL=service")
	wantFailure(t, "service install fails", home, out, err, "service install failed")
	for _, f := range []string{".local/bin/machine-monitor", "cfg/client.json"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s left behind after a failed install", f)
		}
	}
}

func TestInstallShRollbackRestoresReplacedBinary(t *testing.T) {
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	const old = "#!/bin/sh\n# the old binary\n"
	// The home directory is made by installRunWith, so seed it through the command.
	seed := `mkdir -p "$HOME/.local/bin" "$MACHINE_MONITOR_DIR" && printf '%s' "$OLD" > "$HOME/.local/bin/machine-monitor" && ` +
		`chmod 755 "$HOME/.local/bin/machine-monitor" && echo '{"old":1}' > "$MACHINE_MONITOR_DIR/client.json" && `
	home, _, out, err := installRunWith(t, th, th.Token(), cfgDir+seed+oneLiner, "FAKE_FAIL=service", "OLD="+old)
	wantFailure(t, "service install fails", home, out, err, "service install failed")
	if b, _ := os.ReadFile(filepath.Join(home, ".local", "bin", "machine-monitor")); string(b) != old {
		t.Errorf("binary after rollback = %q, want the old one", b)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "cfg", "client.json")); string(b) != "{\"old\":1}\n" {
		t.Errorf("an existing client.json was touched: %q", b)
	}
}

// A symlinked binary and client.json come back as symlinks after a failed install, and a
// dangling link is not deleted (skeptic pass 3, N11).
func TestInstallShRollbackRestoresSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for Unix")
	}
	withRelease(t, "1.2.3")
	th := newTestHub(t)
	seed := `mkdir -p "$HOME/.local/bin" "$MACHINE_MONITOR_DIR" "$HOME/real" && ` +
		`printf old > "$HOME/real/mm" && ln -s ../../real/mm "$HOME/.local/bin/machine-monitor" && ` +
		`ln -s "$HOME/real/nowhere.json" "$MACHINE_MONITOR_DIR/client.json" && `
	home, _, out, err := installRunWith(t, th, th.Token(), cfgDir+seed+oneLiner, "FAKE_FAIL=service")
	wantFailure(t, "service install fails", home, out, err, "service install failed")
	for f, want := range map[string]string{
		".local/bin/machine-monitor": "../../real/mm",
		"cfg/client.json":            filepath.Join(home, "real", "nowhere.json"),
	} {
		if got, err := os.Readlink(filepath.Join(home, f)); err != nil || got != want {
			t.Errorf("%s = %q, %v; want a link to %q", f, got, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, "real", "mm")); string(b) != "old" {
		t.Errorf("the link's target was changed: %q", b)
	}
}
