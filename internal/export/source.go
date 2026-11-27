package export

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
)

const (
	minuteMS  = 60 * 1000
	maxPoints = 3000
	maxBody   = 32 << 20
)

func unixMS(t time.Time) float64 { return float64(t.UnixMilli()) }

type Local struct {
	Dir         string
	DevicesFile string
	Now         func() time.Time
}

func (l Local) Points(_ context.Context, id string, from, to time.Time) ([]history.Point, error) {
	snap, err := history.ReadDir(l.Dir, l.Now)
	if err != nil {
		return nil, sentence("Could not read the history in %s: %v.", l.Dir, err)
	}
	if !snap.Has(id) {
		return nil, &UnknownDeviceError{ID: id, Where: "in this machine's history"}
	}
	return snap.MinutePoints(id, unixMS(from), unixMS(to)), nil
}

type Hub struct {
	URL    string
	Key    string
	Client *http.Client
}

func (h Hub) connection(ctx context.Context) (*http.Client, error) {
	c := agent.NewHTTPClient()
	c.Timeout = 30 * time.Second
	if h.Client != nil {
		*c = *h.Client
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr = tr.Clone()
			tr.Proxy = nil
			c.Transport = tr
		} else if c.Transport == nil {
			c.Transport = agent.NewHTTPClient().Transport
		}
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if h.Key != "" {
		if err := agent.Prove(ctx, c, strings.TrimRight(h.URL, "/"), h.Key); err != nil {
			c.CloseIdleConnections()
			return nil, sentence("Could not verify the hub at %s; the key was not sent: %v.", h.URL, err)
		}
	}
	return c, nil
}

func (h Hub) Points(ctx context.Context, id string, from, to time.Time) ([]history.Point, error) {
	client, err := h.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	base := strings.TrimRight(h.URL, "/")
	fromMS, toMS := unixMS(from), unixMS(to)
	var out []history.Point
	last := math.Inf(-1)
	for start := math.Floor(fromMS/minuteMS) * minuteMS; start < toMS; {
		n := math.Ceil((toMS - start) / minuteMS)
		n = math.Max(10, math.Min(maxPoints, n))
		end := start + n*minuteMS
		res, err := h.get(ctx, client, base, id, start, end, n)
		if err != nil {
			return nil, err
		}
		for _, p := range res.Points {

			if p.T > last && p.T >= fromMS && p.T <= toMS {
				out = append(out, p)
				last = p.T
			}
		}
		start = end
	}
	return out, nil
}

func (h Hub) get(ctx context.Context, client *http.Client, base, id string, from, to, points float64) (*history.Result, error) {
	q := url.Values{}
	q.Set("from", strconv.FormatFloat(from, 'f', -1, 64))
	q.Set("to", strconv.FormatFloat(to, 'f', -1, 64))
	q.Set("points", strconv.FormatFloat(points, 'f', -1, 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/history/"+url.PathEscape(id)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, sentence("%q is not a hub address: %v.", h.URL, err)
	}
	if h.Key != "" {
		req.Header.Set("Authorization", "Bearer "+h.Key)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, sentence("Could not reach the hub at %s: %v.", h.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, sentence("Could not read the reply from the hub at %s: %v.", h.URL, err)
	}
	if len(body) > maxBody {
		return nil, sentence("The hub at %s sent a reply larger than %d MB, which is more than a history request can need.", h.URL, maxBody>>20)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, sentence("The hub at %s is locked and this machine has no key for it. Pair with it first: machine-monitor connect %s", h.URL, h.URL)
	case http.StatusNotFound:
		return nil, &UnknownDeviceError{ID: id, Where: "on the hub at " + h.URL}
	default:
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, sentence("The hub at %s answered %s: %s", h.URL, resp.Status, msg)
	}
	var res history.Result
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, sentence("The hub at %s sent history this version can't read: %v.", h.URL, err)
	}
	return &res, nil
}

func HubURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", sentence("%q is not a hub address; use one like http://192.168.1.10:7777.", s)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/"), nil
}

func KeyFor(clientPath, hubURL string) (string, error) {
	cfg, err := client.LoadConfig(clientPath)
	if err != nil {
		return "", sentence("Could not read %s: %v.", clientPath, err)
	}
	saved, err := HubURL(cfg.Hub)
	if err != nil || saved != hubURL {
		return "", nil
	}
	return cfg.Key, nil
}

type NamedDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Lister interface {
	Devices(ctx context.Context) ([]NamedDevice, error)
}

func (l Local) Devices(_ context.Context) ([]NamedDevice, error) {
	if l.DevicesFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(l.DevicesFile)
	if err != nil {
		return nil, nil
	}
	var saved map[string]struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &saved) != nil {
		return nil, nil
	}
	out := make([]NamedDevice, 0, len(saved))
	for id, d := range saved {
		out = append(out, NamedDevice{ID: id, Name: d.Name})
	}
	return out, nil
}

func (h Hub) Devices(ctx context.Context) ([]NamedDevice, error) {
	client, err := h.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.URL, "/")+"/api/devices?compact=1", nil)
	if err != nil {
		return nil, err
	}
	if h.Key != "" {
		req.Header.Set("Authorization", "Bearer "+h.Key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	var out []NamedDevice
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func ResolveDevice(ctx context.Context, src Source, arg string) (string, error) {
	l, ok := src.(Lister)
	if !ok {
		return arg, nil
	}
	devs, err := l.Devices(ctx)
	if err != nil {
		return arg, nil
	}
	var ids []string
	for _, d := range devs {
		if d.ID == arg {
			return arg, nil
		}
		if strings.EqualFold(d.Name, arg) {
			ids = append(ids, d.ID)
		}
	}
	switch len(ids) {
	case 0:
		return arg, nil
	case 1:
		return ids[0], nil
	}
	sort.Strings(ids)
	return "", sentence("More than one device is named %q; use an id: %s.", arg, strings.Join(ids, ", "))
}
