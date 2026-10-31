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

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
)

const (
	minuteMS  = 60 * 1000
	maxPoints = 3000     // the hub clamps ?points= to [10, 3000]
	maxBody   = 32 << 20 // a 3000-point reply is ~1.5 MB
)

func unixMS(t time.Time) float64 { return float64(t.UnixMilli()) }

// Local reads the history directory a hub on this machine keeps (config.Paths.History). It
// only reads, so it is safe beside a running hub.
type Local struct {
	Dir         string
	DevicesFile string           // devices.json, for looking a device up by name; may be ""
	Now         func() time.Time // clock for the 30-day cutoff; nil means time.Now
}

// Points reads the directory and returns id's stored minutes in [from, to].
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

// Hub reads history from a hub's GET /api/history/:id.
type Hub struct {
	URL    string       // e.g. http://192.168.1.10:7777
	Key    string       // hub key, sent as a Bearer token; empty sends none
	Client *http.Client // nil means a client with a 30 s timeout
}

// Points fetches id's minutes in [from, to]. The hub downsamples to at most 3000 points per
// request, so Points asks for minute-aligned windows of at most 3000 minutes with one point
// per minute, which the hub returns undownsampled, and joins them.
func (h Hub) Points(ctx context.Context, id string, from, to time.Time) ([]history.Point, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
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
			// Windows share their edge minute, and the hub pads past [from, to].
			if p.T > last && p.T >= fromMS && p.T <= toMS {
				out = append(out, p)
				last = p.T
			}
		}
		start = end
	}
	return out, nil
}

// get makes one history request.
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
			err = ue.Err // drop the "Get <full URL>:" prefix
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

// HubURL tidies a --hub value: it adds http:// when no scheme is given and drops a trailing
// slash, so it compares equal to the URL client.json saved.
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

// KeyFor returns the hub key saved in client.json (PLAN §5) for hubURL, or "" when the file
// is missing or was saved for another hub. The key is only ever sent back to the hub it came
// from. It reads the file through client.LoadConfig, as the TUI does, so it accepts every
// spelling of the fields that connect and upstream write.
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

// NamedDevice is a device a source can list: its id and the name people know it by.
type NamedDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Lister is a Source that can also list its devices, so a name can stand in for an id.
type Lister interface {
	Devices(ctx context.Context) ([]NamedDevice, error)
}

// Devices lists the devices in the registry file a hub on this machine keeps
// (config.Paths.Devices), a JSON object keyed by id. It returns nil when the file is not set
// or can't be read: names are a convenience, and the id route still works.
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

// Devices asks the hub for its devices (GET /api/devices).
func (h Hub) Devices(ctx context.Context) ([]NamedDevice, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.URL, "/")+"/api/devices", nil)
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

// ResolveDevice turns what the user typed into a device id. An exact id wins; otherwise a
// name matches without regard to case. When src can't list devices, or nothing matches,
// the argument is used as the id as typed, so the usual "no such device" error still
// applies. Two devices with one name is an error that lists their ids.
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
