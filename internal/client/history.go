package client

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

type Energy struct {
	Bounds   []float64       `json:"bounds"`
	Settings *power.Settings `json:"settings"`
	Devices  []hub.Usage     `json:"devices"`
}

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

func ms(v float64) string { return strconv.FormatFloat(math.Round(v), 'f', -1, 64) }
