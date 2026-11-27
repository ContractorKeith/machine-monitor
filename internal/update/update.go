package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Repo = "ContractorKeith/machine-monitor"

const (
	maxAPI      = 1 << 20
	maxSmall    = 1 << 20
	MaxArchive  = 200 << 20
	maxBinary   = 200 << 20
	httpTimeout = 2 * time.Minute
)

type Source struct {
	API      string
	Download string
	Repo     string
	Trust    Trust
	Client   *http.Client
}

func GitHub() Source {
	return Source{API: "https://api.github.com", Download: "https://github.com", Repo: Repo,
		Trust: DefaultTrust(Repo), Client: &http.Client{Timeout: httpTimeout}}
}

type Release struct {
	Tag       string
	Name      string
	Notes     string
	URL       string
	Published time.Time
}

func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

var tagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)

func ValidTag(tag string) bool { return tagRE.MatchString(tag) }

func ArchiveName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("machine-monitor_%s_%s_%s%s", version, goos, goarch, ext)
}

func (s Source) DownloadBase(tag string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s", strings.TrimRight(s.Download, "/"), s.Repo, tag)
}

func (s Source) AssetURL(tag, name string) string { return s.DownloadBase(tag) + "/" + name }

func (s Source) Latest(ctx context.Context) (Release, error) {
	b, err := s.Fetch(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(s.API, "/"), s.Repo), maxAPI)
	if err != nil {
		return Release{}, err
	}
	var raw struct {
		Tag       string    `json:"tag_name"`
		Name      string    `json:"name"`
		Body      string    `json:"body"`
		URL       string    `json:"html_url"`
		Published time.Time `json:"published_at"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Release{}, fmt.Errorf("latest release: %w", err)
	}
	if !ValidTag(raw.Tag) {
		return Release{}, fmt.Errorf("latest release has an unexpected tag %q", raw.Tag)
	}
	return Release{Tag: raw.Tag, Name: raw.Name, Notes: raw.Body, URL: raw.URL, Published: raw.Published}, nil
}

func (s Source) Checksums(ctx context.Context, tag string) (map[string]string, error) {
	if !ValidTag(tag) {
		return nil, fmt.Errorf("bad release tag %q", tag)
	}
	sums, err := s.Fetch(ctx, s.AssetURL(tag, "checksums.txt"), maxSmall)
	if err != nil {
		return nil, err
	}
	bundle, err := s.Fetch(ctx, s.AssetURL(tag, "checksums.txt.sigstore.json"), maxSmall)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	if err := s.Trust.Verify(sums, bundle, tag); err != nil {
		return nil, fmt.Errorf("checksums.txt signature: %w", err)
	}
	return ParseChecksums(sums)
}

var sumLineRE = regexp.MustCompile(`^([a-f0-9]{64}) [ *]?([A-Za-z0-9._+-]+)$`)

func ParseChecksums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		m := sumLineRE.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("checksums.txt: bad line %q", line)
		}
		out[m[2]] = m[1]
	}
	if len(out) == 0 {
		return nil, errors.New("checksums.txt is empty")
	}
	return out, nil
}

func (s Source) Fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream;q=0.9, */*;q=0.8")
	req.Header.Set("User-Agent", "machine-monitor-update")
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: httpTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, rateLimitError(resp.Header.Get("X-RateLimit-Reset"))
		}
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

func rateLimitError(reset string) error {
	n, err := strconv.ParseInt(reset, 10, 64)
	if err != nil {
		return errors.New("GitHub API rate limit reached; try again in an hour")
	}
	return fmt.Errorf("GitHub API rate limit reached; it resets at %s", time.Unix(n, 0).Format("15:04 MST"))
}

func Newer(latest, current string) bool {
	l, ok := parseVersion(latest)
	if !ok {
		return false
	}
	c, ok := parseVersion(current)
	if !ok || strings.Contains(current, "SNAPSHOT") {
		return true
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func (s Source) Binary(ctx context.Context, tag, goos, goarch string) ([]byte, error) {
	sums, err := s.Checksums(ctx, tag)
	if err != nil {
		return nil, err
	}
	name := ArchiveName(strings.TrimPrefix(tag, "v"), goos, goarch)
	want, ok := sums[name]
	if !ok {
		return nil, fmt.Errorf("release %s has no build for %s/%s", tag, goos, goarch)
	}
	data, err := s.Fetch(ctx, s.AssetURL(tag, name), MaxArchive)
	if err != nil {
		return nil, err
	}
	if err := CheckSHA256(data, want); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return ExtractBinary(name, data)
}
