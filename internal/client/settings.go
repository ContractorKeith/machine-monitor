package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

const EventPaired = "paired"

const EventEnergySettings = "energySettings"

var ErrNotAvailable = errors.New("not available on this hub")

func (c *Client) EnergySettings(ctx context.Context) (*power.Settings, error) {
	var out *power.Settings
	if err := c.do(ctx, "GET", "/api/energy/settings", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) SetEnergySettings(ctx context.Context, s power.Settings) (power.Settings, error) {
	var out power.Settings
	err := c.do(ctx, "PUT", "/api/energy/settings", s, &out)
	return out, err
}

type PairCode struct {
	Code    string `json:"code"`
	Expires int64  `json:"expires"`
}

func (c *Client) NewPairCode(ctx context.Context) (*PairCode, error) {
	var out PairCode
	if err := c.do(ctx, "POST", "/api/pair/code", map[string]string{"owner": c.owner}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Remote struct {
	tailscale.Status
	Port         int    `json:"port"`
	CanConfigure bool   `json:"canConfigure"`
	Platform     string `json:"platform"`
}

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

type Info struct {
	Hostname  string        `json:"hostname"`
	Port      int           `json:"port"`
	Addresses []hub.Address `json:"addresses"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.do(ctx, "GET", "/api/info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

var virtualIface = regexp.MustCompile(`(?i)vEthernet|WSL|VirtualBox|VMware|docker|^br-|^veth|^virbr`)

func (c *Client) InstallURL(info *Info) string {
	host := c.base.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return c.URL()
	}
	if info == nil || len(info.Addresses) == 0 {
		return c.URL()
	}
	addr := info.Addresses[0].Address
	for _, a := range info.Addresses {
		if !virtualIface.MatchString(a.Name) {
			addr = a.Address
			break
		}
	}
	u := *c.base
	u.Host = net.JoinHostPort(addr, c.base.Port())
	return u.String()
}

func (c *Client) InstallCommands(hubURL string) (sh, ps string) {
	sh = `curl -sSL --fail-with-body -H "Authorization: Bearer ` + c.key + `" ` + hubURL + `/install.sh | sh`
	ps = `irm -Headers @{Authorization='Bearer ` + c.key + `'} ` + hubURL + `/install.ps1 | iex`
	return sh, ps
}

type energySettingsEvent struct {
	Settings *power.Settings `json:"settings"`
}

func EnergySettingsOf(ev Event) *power.Settings {
	var e energySettingsEvent
	if json.Unmarshal(ev.Raw, &e) != nil {
		return nil
	}
	return e.Settings
}
