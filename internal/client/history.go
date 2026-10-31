package client

// History and energy reads for the TUI's History and Energy views (PLAN §6, §7 views 4–5).

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
)

// Energy is the body of GET /api/energy: per-device totals between bounds, and the hub's
// saved price (nil until the user sets one).
type Energy struct {
	Bounds   []float64       `json:"bounds"`
	Settings *power.Settings `json:"settings"`
	Devices  []hub.Usage     `json:"devices"`
}

// History is GET /api/history/:id?from=&to=&points=: id's history between from and to (Unix
// ms), downsampled by the hub to about points buckets.
func (c *Client) History(ctx context.Context, id string, from, to float64, points int) (*history.Result, error) {
	q := url.Values{}
	q.Set("from", ms(from))
	q.Set("to", ms(to))
	q.Set("points", strconv.Itoa(points))
	var out history.Result
	if err := c.do(ctx, "GET", "/api/history/"+url.PathEscape(id)+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Energy is GET /api/energy?bounds=: energy, cost and usage totals for every device, bucketed
// between consecutive bounds (Unix ms, ascending, 2–800 of them, at most 32 days in all).
func (c *Client) Energy(ctx context.Context, bounds []float64) (*Energy, error) {
	parts := make([]string, len(bounds))
	for i, b := range bounds {
		parts[i] = ms(b)
	}
	var out Energy
	if err := c.do(ctx, "GET", "/api/energy?bounds="+strings.Join(parts, ","), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ms formats a Unix-ms time as a whole number, as upstream's dashboard sends it.
func ms(v float64) string { return strconv.FormatFloat(math.Round(v), 'f', -1, 64) }
