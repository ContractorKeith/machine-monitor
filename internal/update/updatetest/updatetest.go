// Package updatetest is a fake GitHub release server for tests: real archives, a real
// checksums.txt and a real Sigstore-style bundle signed by a throwaway CA that the returned
// update.Source trusts.
package updatetest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/update"
)

// Release is a fake release server. Change files with Set; re-sign with Sign.
type Release struct {
	Server *httptest.Server
	Source update.Source
	Tag    string
	Notes  string

	t      *testing.T
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate

	mu      sync.Mutex
	files   map[string][]byte
	fetched []string
}

// New serves release tag (e.g. "v1.2.3") with one archive per "goos/goarch" in bins, whose
// machine-monitor binary has the given contents. checksums.txt is signed for tag.
func New(t *testing.T, tag string, bins map[string][]byte) *Release {
	t.Helper()
	r := &Release{t: t, Tag: tag, Notes: "- Faster charts\n- Fixed a crash", files: map[string][]byte{}}
	r.caKey, r.caCert = newCA(t)
	version := strings.TrimPrefix(tag, "v")
	for plat, bin := range bins {
		goos, goarch, _ := strings.Cut(plat, "/")
		name := update.ArchiveName(version, goos, goarch)
		r.files[name] = Archive(t, name, bin)
	}
	r.files["checksums.txt"] = r.checksums()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+update.Repo+"/releases/latest", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": r.Tag, "name": "machine-monitor " + r.Tag, "body": r.Notes,
			"html_url":     r.Server.URL + "/" + update.Repo + "/releases/tag/" + r.Tag,
			"published_at": "2026-10-30T12:00:00Z",
		})
	})
	mux.HandleFunc("GET /"+update.Repo+"/releases/download/{tag}/{name}", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		r.mu.Lock()
		b, ok := r.files[req.PathValue("name")]
		r.mu.Unlock()
		if req.PathValue("tag") != r.Tag || !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(b)
	})
	r.Server = httptest.NewServer(mux)
	t.Cleanup(r.Server.Close)

	roots := x509.NewCertPool()
	roots.AddCert(r.caCert)
	r.Source = update.Source{API: r.Server.URL, Download: r.Server.URL, Repo: update.Repo,
		Trust:  update.Trust{Roots: roots, Issuer: update.GitHubIssuer, Identity: update.WorkflowIdentity(update.Repo)},
		Client: r.Server.Client()}
	r.Sign(update.WorkflowIdentity(update.Repo)(tag), r.caKey, r.caCert)
	return r
}

func (r *Release) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetched = append(r.fetched, req.URL.Path)
}

// Fetched lists the request paths served so far, in order.
func (r *Release) Fetched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fetched...)
}

// File returns a served file.
func (r *Release) File(name string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[name]
}

// Set replaces (or adds) a served file without touching checksums.txt or its signature.
func (r *Release) Set(name string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[name] = data
}

// Remove stops serving a file (it 404s), leaving checksums.txt as it is.
func (r *Release) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, name)
}

// Sign writes checksums.txt.sigstore.json: a leaf certificate for identity issued by caCert,
// and its signature over the current checksums.txt.
func (r *Release) Sign(identity string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate) {
	r.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		r.t.Fatal(err)
	}
	u, err := url.Parse(identity)
	if err != nil {
		r.t.Fatal(err)
	}
	issuer, _ := asn1.MarshalWithParams(update.GitHubIssuer, "utf8")
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(9 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{u},
		ExtraExtensions: []pkix.Extension{
			{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}, Value: issuer},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		r.t.Fatal(err)
	}
	msg := r.File("checksums.txt")
	digest := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"certificate": map[string]any{"rawBytes": der}},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": digest[:]},
			"signature":     sig,
		},
	})
	r.Set("checksums.txt.sigstore.json", b)
}

// NewCA is a throwaway CA the Source does not trust, for bad-signature tests.
func NewCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) { return newCA(t) }

// CA is the CA the Source trusts.
func (r *Release) CA() (*ecdsa.PrivateKey, *x509.Certificate) { return r.caKey, r.caCert }

func newCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test sigstore"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return key, cert
}

// checksums is sha256sum output over every archive.
func (r *Release) checksums() []byte {
	var names []string
	for n := range r.files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		sum := sha256.Sum256(r.files[n])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), n)
	}
	return []byte(b.String())
}

// Archive builds a release archive (zip for .zip names, else tar.gz) holding a LICENSE and
// the binary.
func Archive(t *testing.T, name string, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for _, f := range []struct {
			n string
			b []byte
		}{{"LICENSE", []byte("MIT")}, {"machine-monitor.exe", bin}} {
			w, err := zw.Create(f.n)
			if err != nil {
				t.Fatal(err)
			}
			w.Write(f.b)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		n    string
		mode int64
		b    []byte
	}{{"LICENSE", 0o644, []byte("MIT")}, {"machine-monitor", 0o755, bin}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.n, Mode: f.mode, Size: int64(len(f.b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
