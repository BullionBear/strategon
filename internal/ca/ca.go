// Package ca issues Ed25519 certificates for Strategon mTLS (offline CA).
// Production enrollment (token → CSR) can reuse SignCSR later; this package
// covers the bootstrap tooling path: init a CA, sign leaf certs by CN.
package ca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertName = "ca-cert.pem"
	caKeyName  = "ca-key.pem"
	certName   = "cert.pem"
	keyName    = "key.pem"

	defaultCAValidFor   = 10 * 365 * 24 * time.Hour
	defaultLeafValidFor = 2 * 365 * 24 * time.Hour
)

// Files written by Init / Sign.
type Bundle struct {
	CertPath string
	KeyPath  string
}

// Init creates a new self-signed Ed25519 CA under dir (ca-cert.pem, ca-key.pem).
func Init(dir string) (Bundle, error) {
	if dir == "" {
		return Bundle{}, errors.New("ca output dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Bundle{}, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Bundle{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return Bundle{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "strategon-ca",
			Organization: []string{"Strategon"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(defaultCAValidFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return Bundle{}, err
	}
	certPath := filepath.Join(dir, caCertName)
	keyPath := filepath.Join(dir, caKeyName)
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return Bundle{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Bundle{}, err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return Bundle{}, err
	}
	return Bundle{CertPath: certPath, KeyPath: keyPath}, nil
}

// SignOpts configures a leaf certificate signed by the CA.
type SignOpts struct {
	CADir    string
	OutDir   string
	CN       string
	Server   bool // ServerAuth (+ DNS/IP SANs); otherwise ClientAuth (agent)
	DNSNames []string
	IPs      []net.IP
	ValidFor time.Duration
}

// Sign issues an Ed25519 leaf cert/key under OutDir (cert.pem, key.pem).
func Sign(opts SignOpts) (Bundle, error) {
	if opts.CADir == "" || opts.OutDir == "" {
		return Bundle{}, errors.New("--ca and --out are required")
	}
	if opts.CN == "" {
		return Bundle{}, errors.New("--cn is required")
	}
	caCert, caKey, err := loadCA(opts.CADir)
	if err != nil {
		return Bundle{}, err
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return Bundle{}, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Bundle{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return Bundle{}, err
	}
	validFor := opts.ValidFor
	if validFor <= 0 {
		validFor = defaultLeafValidFor
	}

	dnsNames := append([]string{}, opts.DNSNames...)
	ips := append([]net.IP{}, opts.IPs...)
	if opts.Server {
		if ip := net.ParseIP(opts.CN); ip != nil {
			ips = appendUniqueIP(ips, ip)
		} else if !containsFold(dnsNames, opts.CN) {
			dnsNames = append(dnsNames, opts.CN)
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: opts.CN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	if opts.Server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		return Bundle{}, err
	}
	certPath := filepath.Join(opts.OutDir, certName)
	keyPath := filepath.Join(opts.OutDir, keyName)
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return Bundle{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Bundle{}, err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return Bundle{}, err
	}
	return Bundle{CertPath: certPath, KeyPath: keyPath}, nil
}

func loadCA(dir string) (*x509.Certificate, ed25519.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertName))
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyName))
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("invalid CA key PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, errors.New("CA key is not Ed25519")
	}
	return cert, key, nil
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: typ, Bytes: der})
}

func randomSerial() (*big.Int, error) {
	// 128-bit positive serial.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func containsFold(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func appendUniqueIP(ips []net.IP, ip net.IP) []net.IP {
	for _, existing := range ips {
		if existing.Equal(ip) {
			return ips
		}
	}
	return append(ips, ip)
}
