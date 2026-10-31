package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
)

// TestSettingsAgainstHub: the price round-trips through GET/PUT /api/energy/settings and
// arrives on the stream; a pairing code is six digits valid for about ten minutes; the
// keyed info lists the hub's port; /api/remote is read or reported as not available.
func TestSettingsAgainstHub(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	c := th.client(th.h.Token())
	ctx := context.Background()

	if s, err := c.EnergySettings(ctx); err != nil || s != nil {
		t.Fatalf("EnergySettings before any = %+v, %v", s, err)
	}
	ev := stream(t, c, fastConfig())
	ev.next(EventSnapshot)
	in := power.Settings{Region: "custom", Rate: 0.21234, Currency: "EUR",
		TOU: []power.Period{{From: 16, To: 21, Rate: 0.4}, {From: 0, To: 6, Rate: 0.1}}}
	saved, err := c.SetEnergySettings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Rate != 0.2123 || saved.TOU[0].From != 0 { // rounded and sorted by the hub
		t.Errorf("saved = %+v", saved)
	}
	if got, err := c.EnergySettings(ctx); err != nil || got == nil || got.Currency != "EUR" || len(got.TOU) != 2 {
		t.Errorf("EnergySettings = %+v, %v", got, err)
	}
	if s := EnergySettingsOf(ev.next(EventEnergySettings)); s == nil || s.Rate != 0.2123 {
		t.Errorf("energySettings event = %+v", s)
	}
	var se *StatusError
	if _, err := c.SetEnergySettings(ctx, power.Settings{Region: "custom", Rate: -1, Currency: "EUR"}); !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("bad settings: %v", err)
	}

	code, err := c.NewPairCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	left := time.Until(time.UnixMilli(code.Expires))
	if len(code.Code) != 6 || strings.Trim(code.Code, "0123456789") != "" || left < 9*time.Minute || left > 11*time.Minute {
		t.Errorf("code = %+v (%v left)", code, left)
	}

	info, err := c.Info(ctx)
	if err != nil || info.Hostname != "testbox" {
		t.Errorf("Info = %+v, %v", info, err)
	}

	if r, err := c.Remote(ctx); err != nil && !errors.Is(err, ErrNotAvailable) {
		t.Errorf("Remote = %+v, %v", r, err)
	}
}

// TestInstallCommands: the one-liners are the README's, built from a LAN address when
// the client itself uses loopback.
func TestInstallCommands(t *testing.T) {
	c, err := New("http://127.0.0.1:7777", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	info := &Info{Addresses: []hub.Address{{Name: "eth0", Address: "192.168.1.5"}}}
	u := c.InstallURL(info)
	if u != "http://192.168.1.5:7777" {
		t.Errorf("InstallURL = %q", u)
	}
	if c.InstallURL(nil) != "http://127.0.0.1:7777" {
		t.Errorf("InstallURL(nil) = %q", c.InstallURL(nil))
	}
	sh, ps := c.InstallCommands(u)
	if sh != `curl -sSL --fail-with-body -H "Authorization: Bearer abc123" http://192.168.1.5:7777/install.sh | sh` {
		t.Errorf("sh = %s", sh)
	}
	if ps != `irm -Headers @{Authorization='Bearer abc123'} http://192.168.1.5:7777/install.ps1 | iex` {
		t.Errorf("ps = %s", ps)
	}
	lan, _ := New("http://nas.lan:9000", "k")
	if got := lan.InstallURL(info); got != "http://nas.lan:9000" {
		t.Errorf("InstallURL for a LAN name = %q", got)
	}
}
