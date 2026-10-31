package client

// Settings and Add device (PLAN §6, §7): energy price settings, pairing codes, the hub's
// Tailscale status and the one-line agent installs.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

// EventEnergySettings is the stream event the hub sends when the price changes; its Raw is
// {"settings": power.Settings}.
const EventEnergySettings = "energySettings"

// ErrNotAvailable means the hub doesn't have the endpoint (an older hub, or upstream's
// without the feature).
var ErrNotAvailable = errors.New("not available on this hub")

// EnergySettings is GET /api/energy/settings: the saved price, or nil when none is saved.
func (c *Client) EnergySettings(ctx context.Context) (*power.Settings, error) {
	var out *power.Settings
	if err := c.do(ctx, "GET", "/api/energy/settings", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnergySettings is PUT /api/energy/settings. It returns the price as the hub saved it
// (rates rounded, periods sorted); the hub answers 400 for anything power.ParseSettings
// refuses.
func (c *Client) SetEnergySettings(ctx context.Context, s power.Settings) (power.Settings, error) {
	var out power.Settings
	err := c.do(ctx, "PUT", "/api/energy/settings", s, &out)
	return out, err
}

// PairCode is a pairing code from POST /api/pair/code; Expires is Unix ms.
type PairCode struct {
	Code    string `json:"code"`
	Expires int64  `json:"expires"`
}

// NewPairCode is POST /api/pair/code: a fresh 6-digit code, replacing any earlier one.
func (c *Client) NewPairCode(ctx context.Context) (*PairCode, error) {
	var out PairCode
	if err := c.do(ctx, "POST", "/api/pair/code", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Remote is GET /api/remote, upstream's Tailscale status of the hub's machine.
type Remote struct {
	tailscale.Status
	Port int `json:"port"`
}

// Remote is GET /api/remote. It returns ErrNotAvailable when the hub has no such endpoint.
func (c *Client) Remote(ctx context.Context) (*Remote, error) {
	var out Remote
	if err := c.do(ctx, "GET", "/api/remote", nil, &out); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil, ErrNotAvailable
		}
		return nil, err
	}
	return &out, nil
}

// Info is the keyed GET /api/info: the hub's name, port and LAN addresses.
type Info struct {
	Hostname  string        `json:"hostname"`
	Port      int           `json:"port"`
	Addresses []hub.Address `json:"addresses"`
}

// Info is GET /api/info with the key.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.do(ctx, "GET", "/api/info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InstallURL is the hub address other machines should use: the URL this client uses,
// unless that is a loopback address, which only works on the hub's own machine; then the
// first LAN address info lists, with the same port.
func (c *Client) InstallURL(info *Info) string {
	host := c.base.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return c.URL()
	}
	if info == nil || len(info.Addresses) == 0 {
		return c.URL()
	}
	u := *c.base
	u.Host = net.JoinHostPort(info.Addresses[0].Address, c.base.Port())
	return u.String()
}

// InstallCommands are the one-line agent installs for hubURL, as the README gives them:
// for Linux and macOS (sh) and for Windows PowerShell (ps). They carry the hub key.
func (c *Client) InstallCommands(hubURL string) (sh, ps string) {
	sh = `curl -sSL --fail-with-body -H "Authorization: Bearer ` + c.key + `" ` + hubURL + `/install.sh | sh`
	ps = `irm -Headers @{Authorization='Bearer ` + c.key + `'} ` + hubURL + `/install.ps1 | iex`
	return sh, ps
}

// energySettingsEvent is an energySettings event's body.
type energySettingsEvent struct {
	Settings *power.Settings `json:"settings"`
}

// EnergySettingsOf reads the price from an energySettings event; nil when it has none.
func EnergySettingsOf(ev Event) *power.Settings {
	var e energySettingsEvent
	if json.Unmarshal(ev.Raw, &e) != nil {
		return nil
	}
	return e.Settings
}
