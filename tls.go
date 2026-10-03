package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

const letsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

// webTLS is what the HTTP server needs for the configured TLS mode.
type webTLS struct {
	Config *tls.Config       // nil when mode is "off"
	ACME   *autocert.Manager // set in mode "acme"
	leaf   func() *x509.Certificate
}

// Fingerprint is the SHA-256 of the served certificate, for pinning in the
// iOS app. Empty for Let's Encrypt, whose certificate is trusted anyway.
func (w *webTLS) Fingerprint() string {
	if w == nil || w.leaf == nil {
		return ""
	}
	c := w.leaf()
	if c == nil {
		return ""
	}
	sum := sha256.Sum256(c.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func setupTLS(c *Config, dataDir string) (*webTLS, error) {
	t := c.Web.TLS
	switch t.Mode {
	case "off":
		return &webTLS{}, nil

	case "acme":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(dataDir, "acme")),
			HostPolicy: autocert.HostWhitelist(t.Domain),
			Email:      t.Email,
		}
		if t.Staging {
			m.Client = &acme.Client{DirectoryURL: letsEncryptStaging}
		}
		cfg := m.TLSConfig() // answers tls-alpn-01 challenges on :443
		cfg.MinVersion = tls.VersionTLS12
		return &webTLS{Config: cfg, ACME: m}, nil

	case "files":
		r := &certReloader{certFile: t.CertFile, keyFile: t.KeyFile}
		if _, err := r.get(); err != nil {
			return nil, err
		}
		return &webTLS{
			Config: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.get() }},
			leaf:   r.leaf,
		}, nil

	case "selfsigned":
		cert, err := loadOrCreateSelfSigned(filepath.Join(dataDir, "tls"), c)
		if err != nil {
			return nil, err
		}
		return &webTLS{
			Config: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
			leaf:   func() *x509.Certificate { return cert.Leaf },
		}, nil
	}
	return nil, fmt.Errorf("unknown tls mode %q", t.Mode)
}

// certReloader re-reads certificate files when they change on disk, so an
// external renewal (e.g. certbot) is picked up without a restart.
type certReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	mtime             time.Time
}

func (r *certReloader) get() (*tls.Certificate, error) {
	st, err := os.Stat(r.certFile)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil && st.ModTime().Equal(r.mtime) {
		return r.cert, nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, err
	}
	r.cert, r.mtime = &c, st.ModTime()
	return r.cert, nil
}

func (r *certReloader) leaf() *x509.Certificate {
	c, err := r.get()
	if err != nil {
		return nil
	}
	return c.Leaf
}

func loadOrCreateSelfSigned(dir string, c *Config) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "selfsigned.crt"), filepath.Join(dir, "selfsigned.key")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return cert, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	name := c.Server.Endpoint
	if name == "" {
		name, _ = os.Hostname()
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name, Organization: []string{appName}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	for _, n := range []string{c.Server.Endpoint, c.Web.TLS.Domain} {
		if n == "" {
			continue
		}
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}
