package server

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// certReloader serves the control listener's certificate and picks up renewals
// without a restart.
//
// ACME clients rewrite the certificate files every couple of months. Loading
// once at startup would mean the control port silently serving an expired
// certificate until someone noticed, so we re-read whenever the files change on
// disk.
type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

// newCertReloader loads the keypair once so that a bad path fails at startup
// rather than on the first agent connection.
func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// tlsConfig returns a config that consults the reloader per handshake.
func (r *certReloader) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.getCertificate,
	}
}

func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.load()
}

// load returns the cached certificate, re-reading it if either file's
// modification time moved.
func (r *certReloader) load() (*tls.Certificate, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return r.cached(fmt.Errorf("stat TLS certificate: %w", err))
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return r.cached(fmt.Errorf("stat TLS key: %w", err))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cert != nil && certInfo.ModTime().Equal(r.certMod) && keyInfo.ModTime().Equal(r.keyMod) {
		return r.cert, nil
	}

	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			// A half-written renewal is transient; keep serving the old
			// certificate rather than dropping every agent.
			return r.cert, nil
		}
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	r.cert, r.certMod, r.keyMod = &cert, certInfo.ModTime(), keyInfo.ModTime()
	return r.cert, nil
}

// cached falls back to the last good certificate when the files are briefly
// unreadable, and only surfaces err if there is nothing to fall back to.
func (r *certReloader) cached(err error) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil {
		return r.cert, nil
	}
	return nil, err
}
