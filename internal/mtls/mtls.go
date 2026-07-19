// Package mtls loads Ed25519 PEM material and builds TLS configs for the
// agent ↔ control-plane AgentService path. Human API auth is orthogonal.
package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
)

type peerCNKey struct{}

// ContextWithPeerCN stores the verified client certificate CN.
func ContextWithPeerCN(ctx context.Context, cn string) context.Context {
	return context.WithValue(ctx, peerCNKey{}, cn)
}

// PeerCN returns the client cert CN when the request was mTLS-authenticated.
func PeerCN(ctx context.Context) (string, bool) {
	cn, ok := ctx.Value(peerCNKey{}).(string)
	if !ok || cn == "" {
		return "", false
	}
	return cn, true
}

// PeerCNHandler injects the peer certificate CommonName into the request
// context (empty when the connection is plaintext / no client cert).
func PeerCNHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		next.ServeHTTP(w, r.WithContext(ContextWithPeerCN(r.Context(), cn)))
	})
}

// LoadCert loads a certificate/key PEM pair.
func LoadCert(certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load tls cert: %w", err)
	}
	return cert, nil
}

// LoadCAPool loads one or more PEM certificates into a pool.
func LoadCAPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("no certificates found in CA file")
	}
	return pool, nil
}

// ServerConfig requires and verifies client certificates against clientCA.
func ServerConfig(cert tls.Certificate, clientCA *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    clientCA,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
}

// ClientConfig presents a client certificate and trusts serverCA.
func ClientConfig(cert tls.Certificate, serverCA *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      serverCA,
		MinVersion:   tls.VersionTLS12,
	}
}

// CertCN returns the leaf certificate CommonName.
func CertCN(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", errors.New("empty certificate")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", err
	}
	if parsed.Subject.CommonName == "" {
		return "", errors.New("certificate has empty CommonName")
	}
	return parsed.Subject.CommonName, nil
}
