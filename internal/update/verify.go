package update

// Sigstore bundle check for checksums.txt, with the standard library only (sigstore-go
// would pull in a large dependency tree for one signature).
//
// The release workflow signs checksums.txt with `cosign sign-blob --bundle` using keyless
// signing: Fulcio issues a short-lived certificate for the workflow's GitHub OIDC identity.
// Verify checks that
//   - the certificate chains to the Fulcio root (fulcio.pem, from sigstore/root-signing's
//     trusted_root.json) and is for code signing,
//   - its OIDC issuer is GitHub Actions and its identity is release.yml in this repo at
//     exactly the release tag, and
//   - its key signed these exact bytes.
//
// It does not check the Rekor transparency-log entry. The chain is checked at the
// certificate's own issue time, since Fulcio certificates live ten minutes; the signing key
// is ephemeral and never leaves the workflow run.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"slices"
)

// GitHubIssuer is the OIDC issuer of GitHub Actions tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// Fulcio certificate extensions carrying the OIDC issuer: v1 holds the raw string, v2 a
// DER UTF8String.
var (
	oidIssuerV1 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
)

//go:embed fulcio.pem
var fulcioPEM []byte

// Trust says who may sign a release's checksums.txt.
type Trust struct {
	Roots         *x509.CertPool
	Intermediates *x509.CertPool
	Issuer        string // OIDC issuer the certificate must carry
	// Identity is the certificate's URI SAN for a tag, e.g.
	// https://github.com/owner/repo/.github/workflows/release.yml@refs/tags/v1.2.3
	Identity func(tag string) string
}

// DefaultTrust trusts the public Fulcio CA for repo's release.yml workflow.
func DefaultTrust(repo string) Trust {
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	certs, err := parsePEMCerts(fulcioPEM)
	if err != nil {
		panic("update: bad embedded fulcio.pem: " + err.Error())
	}
	for _, c := range certs {
		if bytes.Equal(c.RawIssuer, c.RawSubject) {
			roots.AddCert(c)
		} else {
			inter.AddCert(c)
		}
	}
	return Trust{Roots: roots, Intermediates: inter, Issuer: GitHubIssuer, Identity: WorkflowIdentity(repo)}
}

// WorkflowIdentity is the certificate identity of repo's release.yml run for a tag.
func WorkflowIdentity(repo string) func(tag string) string {
	return func(tag string) string {
		return "https://github.com/" + repo + "/.github/workflows/release.yml@refs/tags/" + tag
	}
}

// bundle is the part of a Sigstore bundle (v0.1–v0.3 JSON) that Verify reads. []byte fields
// decode from base64.
type bundle struct {
	VerificationMaterial struct {
		Certificate *struct {
			RawBytes []byte `json:"rawBytes"`
		} `json:"certificate"`
		X509CertificateChain *struct {
			Certificates []struct {
				RawBytes []byte `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"x509CertificateChain"`
	} `json:"verificationMaterial"`
	MessageSignature *struct {
		MessageDigest struct {
			Algorithm string `json:"algorithm"`
			Digest    []byte `json:"digest"`
		} `json:"messageDigest"`
		Signature []byte `json:"signature"`
	} `json:"messageSignature"`
}

// Verify checks that bundleJSON is a valid signature over msg by the release workflow at tag.
func (t Trust) Verify(msg, bundleJSON []byte, tag string) error {
	var b bundle
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return fmt.Errorf("bad bundle: %w", err)
	}
	var der []byte
	switch vm := b.VerificationMaterial; {
	case vm.Certificate != nil:
		der = vm.Certificate.RawBytes
	case vm.X509CertificateChain != nil && len(vm.X509CertificateChain.Certificates) > 0:
		der = vm.X509CertificateChain.Certificates[0].RawBytes
	}
	if len(der) == 0 || b.MessageSignature == nil || len(b.MessageSignature.Signature) == 0 {
		return errors.New("bundle has no certificate or signature")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("bad certificate: %w", err)
	}

	if t.Roots == nil {
		return errors.New("no trusted roots")
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:         t.Roots,
		Intermediates: t.Intermediates,
		CurrentTime:   cert.NotBefore,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	})
	if err != nil {
		return fmt.Errorf("certificate not issued by a trusted CA: %w", err)
	}
	if got := certIssuer(cert); got != t.Issuer {
		return fmt.Errorf("signed through issuer %q, want %q", got, t.Issuer)
	}
	want := t.Identity(tag)
	if !slices.ContainsFunc(cert.URIs, func(u *url.URL) bool { return u.String() == want }) {
		return fmt.Errorf("signed by %s, want %s", identities(cert), want)
	}

	digest := sha256.Sum256(msg)
	if md := b.MessageSignature.MessageDigest; len(md.Digest) > 0 && !bytes.Equal(md.Digest, digest[:]) {
		return errors.New("bundle is for different contents")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("unsupported key type %T", cert.PublicKey)
	}
	if !ecdsa.VerifyASN1(pub, digest[:], b.MessageSignature.Signature) {
		return errors.New("signature does not match")
	}
	return nil
}

// certIssuer is the OIDC issuer Fulcio recorded in cert, or "".
func certIssuer(cert *x509.Certificate) string {
	for _, e := range cert.Extensions {
		if e.Id.Equal(oidIssuerV2) {
			var s string
			if _, err := asn1.UnmarshalWithParams(e.Value, &s, "utf8"); err == nil {
				return s
			}
		}
	}
	for _, e := range cert.Extensions {
		if e.Id.Equal(oidIssuerV1) {
			return string(e.Value)
		}
	}
	return ""
}

// identities lists cert's URI and email SANs for an error message.
func identities(cert *x509.Certificate) string {
	var ids []string
	for _, u := range cert.URIs {
		ids = append(ids, u.String())
	}
	ids = append(ids, cert.EmailAddresses...)
	if len(ids) == 0 {
		return "an unknown identity"
	}
	return fmt.Sprint(ids)
}

func parsePEMCerts(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificates")
	}
	return out, nil
}
