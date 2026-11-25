// Package testenv holds the controlled mocks for cuttle's tests (design 0007
// D2-D5): a generated CA set, origins that record what they receive, the
// test network behind the Network seam, a listener that reports chosen peer
// addresses, the curl helper, and the binary-test sandbox. cmd/cuttle never
// imports it; the T0 gate checks that.
package testenv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a generated root that signs leaves. Keys stay in memory; only the
// root certificate is written, as PEM in a temporary directory, for curl.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
	// LeafKey, when set, is the key of every leaf the CA issues, so a
	// client can pin the leaves' SPKI, as tools/mockenv does for Chrome's
	// --ignore-certificate-errors-spki-list. Otherwise each leaf gets a
	// new key.
	LeafKey *ecdsa.PrivateKey
}

// LeafValidity is the validity of generated leaves.
const LeafValidity = 24 * time.Hour

// NewCA generates an ECDSA P-256 root.
func NewCA(t testing.TB, name string) *CA {
	t.Helper()
	ca, err := GenerateCA(name)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// GenerateCA is NewCA for code outside a test, such as tools/mockenv.
func GenerateCA(name string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: name, Organization: []string{"cuttle tests"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(LeafValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// Leaf issues a server certificate for names, which may be DNS names or IP
// addresses, with the serverAuth EKU and 24-hour validity.
func (ca *CA) Leaf(t testing.TB, names ...string) tls.Certificate {
	t.Helper()
	leaf, err := ca.Issue(names...)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// Issue is Leaf for code that runs outside the test goroutine, such as a
// mock that signs a leaf per TLS handshake.
func (ca *CA) Issue(names ...string) (tls.Certificate, error) {
	key := ca.LeafKey
	if key == nil {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return tls.Certificate{}, err
		}
	}
	sn, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(LeafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Pool returns a pool holding only this root.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// WritePEM writes the root certificate to a file in a new temporary
// directory and returns its path.
func (ca *CA) WritePEM(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, ca.PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
