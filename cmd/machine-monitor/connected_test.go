package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

func TestConnected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	var opened *tui.Options
	runHubTUI = func(o tui.Options, _ io.Writer) int { opened = &o; return 0 }

	try := func(args ...string) (int, string) {
		opened = nil
		var errOut bytes.Buffer
		return run(args, io.Discard, &errOut), errOut.String()
	}
	if code, msg := try("--hub", "http://hub.lan:7777"); code != 2 || !strings.Contains(msg, "no key for") || opened != nil {
		t.Errorf("no key: %d %q", code, msg)
	}
	if code, msg := try("--hub", "ftp://x"); code != 2 || !strings.Contains(msg, "scheme") {
		t.Errorf("bad URL: %d %q", code, msg)
	}
	if code, _ := try("--hub", "hub.lan", "--key", "k1", "--ascii"); code != 0 || opened == nil || opened.Hub == nil || !opened.ASCII {
		t.Fatalf("--key: %d %+v", code, opened)
	}
	if h := opened.Hub.(*client.Client).Host(); h != "hub.lan:7777" {
		t.Errorf("host = %s", h)
	}

	// client.json's key is used only for the hub it belongs to.
	if err := client.SaveConfig(filepath.Join(dir, "client.json"), client.Config{Hub: "http://hub.lan:7777", Key: "saved"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := try("--hub", "hub.lan"); code != 0 || opened == nil {
		t.Errorf("saved key not used: %d", code)
	}
	if code, msg := try("--hub", "other.lan"); code != 2 || !strings.Contains(msg, "no key for") {
		t.Errorf("another hub's saved key was used: %d %q", code, msg)
	}
}
