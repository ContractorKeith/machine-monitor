// Package update finds machine-monitor releases on GitHub, checks them, and replaces the
// running binary (PLAN §3 rows 33 and 35).
//
// A release is trusted only through checksums.txt: its Sigstore bundle must verify against
// the Fulcio root and this repo's release workflow (verify.go), and every archive must match
// its SHA-256 line. `machine-monitor update` and the hub's installers both go through
// Source.Checksums.
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

// Repo is where releases are published.
const Repo = "ContractorKeith/machine-monitor"

// Size limits for downloads.
const (
	maxAPI      = 1 << 20   // release JSON
	maxSmall    = 1 << 20   // checksums.txt and its bundle
	MaxArchive  = 200 << 20 // one release archive
	maxBinary   = 200 << 20 // the binary inside it
	httpTimeout = 2 * time.Minute
)

// Source is a place releases come from. GitHub returns the real one; tests point API and
// Download at a fake server and Trust at a test CA.
type Source struct {
	API      string // GitHub API base, e.g. https://api.github.com
	Download string // release download base, e.g. https://github.com
	Repo     string // owner/name
	Trust    Trust  // who may sign checksums.txt
	Client   *http.Client
}

// GitHub is the public release source for Repo.
func GitHub() Source {
	return Source{API: "https://api.github.com", Download: "https://github.com", Repo: Repo,
		Trust: DefaultTrust(Repo), Client: &http.Client{Timeout: httpTimeout}}
}

// Release is one published release.
type Release struct {
	Tag       string    // "v1.2.3"
	Name      string    // release title
	Notes     string    // release notes (markdown)
	URL       string    // release page
	Published time.Time // zero when unknown
}

// Version is the tag without its leading "v", as goreleaser names archives.
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// tagRE is what a release tag may look like; anything else is refused before it lands in a
// URL or an installer script.
var tagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)

// ValidTag reports whether tag looks like a release tag ("v1.2.3", "v1.2.3-rc.1").
func ValidTag(tag string) bool { return tagRE.MatchString(tag) }

// ArchiveName is the goreleaser archive for a version and platform; it must match
// archives.name_template in .goreleaser.yaml.
func ArchiveName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("machine-monitor_%s_%s_%s%s", version, goos, goarch, ext)
}

// DownloadBase is the URL a release's files are under; AssetURL appends "/<name>".
func (s Source) DownloadBase(tag string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s", strings.TrimRight(s.Download, "/"), s.Repo, tag)
}

// AssetURL is the download URL of a release file.
func (s Source) AssetURL(tag, name string) string { return s.DownloadBase(tag) + "/" + name }

// Latest is the newest published (non-draft, non-prerelease) release.
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

// Checksums downloads checksums.txt for tag and its Sigstore bundle, verifies the signature
// (it must come from this repo's release workflow at exactly this tag), and returns the
// archive name -> lowercase hex SHA-256 map.
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

// ParseChecksums reads sha256sum output ("<hex>  <name>" per line). Any malformed line is
// an error: the file is signed, so damage means something is wrong.
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

// Fetch GETs url and returns at most limit bytes; a longer body is an error.
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

// Newer reports whether release version latest is newer than current. Both may carry a
// leading "v". A current version that isn't a release ("dev", a snapshot) is always older.
// Pre-release suffixes are ignored, so 1.2.3-rc.1 and 1.2.3 compare equal.
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

// Binary downloads the goos/goarch archive of release tag, checks it against the signed
// checksums.txt, and returns the machine-monitor binary inside.
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
