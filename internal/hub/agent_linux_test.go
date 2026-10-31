//go:build linux

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// agentSHA256 pins testdata/upstream/agent.sh to upstream's file at c3a4c41.
const agentSHA256 = "7e4a2d66518939e660055e5790dbef9d92ab134ef3a0fad24f306824b93c292d"

// TestUpstreamAgentReports is the M3 acceptance check: upstream device-monitor's agent.sh, run
// unmodified with bash, reports to this hub and shows up in /api/devices.
func TestUpstreamAgentReports(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real agent for a few seconds")
	}
	const script = "testdata/upstream/agent.sh"
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != agentSHA256 {
		t.Fatalf("%s is not upstream's agent.sh (sha256 %x)", script, sum)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}

	// Real clock: the agent is a real process.
	h, err := New(Options{Paths: config.At(t.TempDir()), Hostname: "testbox", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, script, srv.URL, h.Token())
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "INTERVAL=1", "LC_ALL=C"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so its sleep and curl children die with it
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	}()

	th := &testHub{Hub: h, t: t, h: h.Handler()}
	for {
		if devs := th.devices(); len(devs) > 0 {
			d := devs[0]
			if len(devs) != 1 || !d.Online || d.Platform != "linux" || d.Latest == nil {
				t.Fatalf("devices = %+v", devs)
			}
			l := d.Latest
			if l.CPU.Threads == nil || *l.CPU.Threads < 1 || l.Memory.Total == nil || *l.Memory.Total <= 0 ||
				l.Hostname == nil || l.PowerEst == nil {
				t.Errorf("latest report missing basics: %+v", l.Report)
			}
			t.Logf("upstream agent registered as %s (%s, %s)", d.ID, d.Name, deref(d.OS))
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("upstream agent.sh never showed up in /api/devices")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
