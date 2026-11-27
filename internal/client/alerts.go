package client

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
)

const EventAlertSettings = "alerts"

type Alerts struct {
	Settings alerts.Settings `json:"settings"`
	Active   []alerts.Active `json:"active"`
	Events   []alerts.Event  `json:"events"`
	PriceSet bool            `json:"priceSet"`
}

func (c *Client) Alerts(ctx context.Context) (*Alerts, error) {
	var out Alerts
	if err := c.do(ctx, "GET", "/api/alerts", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SetAlerts(ctx context.Context, s alerts.Settings) (alerts.Settings, []alerts.Active, error) {
	if s.Rules == nil {
		s.Rules = []alerts.Rule{}
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

func (c *Client) TestAlerts(ctx context.Context) (alerts.Delivery, error) {
	var out alerts.Delivery
	err := c.do(ctx, "POST", "/api/alerts/test", nil, &out)
	return out, err
}

func (c *Client) ClearAlertLog(ctx context.Context) error {
	return c.do(ctx, "DELETE", "/api/alerts/log", nil, nil)
}
