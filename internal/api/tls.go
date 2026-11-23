package api

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fingerprint returns the pin for a CA certificate in PEM form.
func Fingerprint(caPEM []byte) (string, error) {
	b, _ := pem.Decode(caPEM)
	if b == nil {
		return "", errors.New("not a PEM certificate")
	}
	sum := sha256.Sum256(b.Bytes)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PinnedTLS returns a client TLS config that trusts only servers holding
// a certificate issued by the given CA for server authentication.
//
// Host names are deliberately not checked: the CA is private to one
// better-backup server, so a valid server certificate identifies that
// server whatever address it is reached by. Requiring the server-auth
// usage stops an agent's client certificate (issued by the same CA) from
// being used to impersonate the server.
func PinnedTLS(caPEM []byte, client *tls.Certificate) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid CA certificate")
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("server sent no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			inter := x509.NewCertPool()
			for _, r := range raw[1:] {
				if c, err := x509.ParseCertificate(r); err == nil {
					inter.AddCert(c)
				}
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			if err != nil {
				return fmt.Errorf("server certificate was not issued by the pinned better-backup CA: %w", err)
			}
			return nil
		},
	}
	if client != nil {
		cfg.Certificates = []tls.Certificate{*client}
	}
	return cfg, nil
}

// FetchCA downloads the server's CA certificate and checks it against the
// fingerprint the administrator supplied. The connection itself is not
// trusted; the fingerprint is what authenticates the result.
func FetchCA(serverURL, fingerprint string) ([]byte, error) {
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := c.Get(strings.TrimRight(serverURL, "/") + "/ca.pem")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetching CA certificate: %s", resp.Status)
	}
	caPEM, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	got, err := Fingerprint(caPEM)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(got, strings.TrimSpace(fingerprint)) {
		return nil, fmt.Errorf("server CA fingerprint is %s, expected %s: wrong server or an intercepted connection", got, fingerprint)
	}
	return caPEM, nil
}
