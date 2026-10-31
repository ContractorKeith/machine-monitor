package client

// Alerts (PLAN §6): GET/PUT /api/alerts, POST /api/alerts/test, DELETE /api/alerts/log.

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
)

// EventAlertSettings is the stream event the hub sends when alert settings change; its Raw
// is an alerts.Message with Settings and Active. EventAlert's Raw is an alerts.Message with
// Event and Active.
const EventAlertSettings = "alerts"

// Alerts is GET /api/alerts: the settings, what is firing (newest first), the latest log
// entries (newest first) and whether an electricity price is set (dailyCost needs one).
type Alerts struct {
	Settings alerts.Settings `json:"settings"`
	Active   []alerts.Active `json:"active"`
	Events   []alerts.Event  `json:"events"`
	PriceSet bool            `json:"priceSet"`
}

// Alerts fetches the hub's alert settings, active alerts and log.
func (c *Client) Alerts(ctx context.Context) (*Alerts, error) {
	var out Alerts
	if err := c.do(ctx, "GET", "/api/alerts", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAlerts is PUT /api/alerts: it replaces the rules and channels and returns them as the
// hub saved them (new rules get ids) with the alerts now active. The hub answers 400 when
// any rule or channel is invalid; alerts.ParseSettings says which first.
func (c *Client) SetAlerts(ctx context.Context, s alerts.Settings) (alerts.Settings, []alerts.Active, error) {
	if s.Rules == nil {
		s.Rules = []alerts.Rule{} // the hub refuses a missing rules list
	}
	var out struct {
		Settings alerts.Settings `json:"settings"`
		Active   []alerts.Active `json:"active"`
	}
	if err := c.do(ctx, "PUT", "/api/alerts", s, &out); err != nil {
		return alerts.Settings{}, nil, err
	}
	return out.Settings, out.Active, nil
}

// TestAlerts is POST /api/alerts/test: the hub broadcasts a test alert and sends it to every
// enabled channel, and reports how each delivery went.
func (c *Client) TestAlerts(ctx context.Context) (alerts.Delivery, error) {
	var out alerts.Delivery
	err := c.do(ctx, "POST", "/api/alerts/test", nil, &out)
	return out, err
}

// ClearAlertLog is DELETE /api/alerts/log.
func (c *Client) ClearAlertLog(ctx context.Context) error {
	return c.do(ctx, "DELETE", "/api/alerts/log", nil, nil)
}
