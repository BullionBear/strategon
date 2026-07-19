package ca

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestInitAndSignClientServerEd25519(t *testing.T) {
	root := t.TempDir()
	caDir := filepath.Join(root, "ca")
	if _, err := Init(caDir); err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(root, "m1")
	if _, err := Sign(SignOpts{CADir: caDir, OutDir: agentDir, CN: "m1"}); err != nil {
		t.Fatal(err)
	}
	cpDir := filepath.Join(root, "cp")
	if _, err := Sign(SignOpts{
		CADir: caDir, OutDir: cpDir, CN: "control-plane", Server: true,
		DNSNames: []string{"cp.internal"},
		IPs:      []net.IP{net.ParseIP("127.0.0.1")},
	}); err != nil {
		t.Fatal(err)
	}

	assertEd25519Leaf(t, filepath.Join(agentDir, certName), filepath.Join(agentDir, keyName), false, "m1")
	assertEd25519Leaf(t, filepath.Join(cpDir, certName), filepath.Join(cpDir, keyName), true, "control-plane")

	caPool := x509.NewCertPool()
	caPEM, err := os.ReadFile(filepath.Join(caDir, caCertName))
	if err != nil {
		t.Fatal(err)
	}
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA")
	}
	clientCert, err := tls.LoadX509KeyPair(filepath.Join(agentDir, certName), filepath.Join(agentDir, keyName))
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.LoadX509KeyPair(filepath.Join(cpDir, certName), filepath.Join(cpDir, keyName))
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			errCh <- err
			return
		}
		if got := tlsConn.ConnectionState().PeerCertificates[0].Subject.CommonName; got != "m1" {
			errCh <- fmt.Errorf("peer CN=%q", got)
			return
		}
		errCh <- nil
	}()

	client, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		ServerName:   "cp.internal",
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func assertEd25519Leaf(t *testing.T, certPath, keyPath string, server bool, cn string) {
	t.Helper()
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cert.PublicKey.(ed25519.PublicKey); !ok {
		t.Fatalf("leaf public key is %T, want ed25519", cert.PublicKey)
	}
	if cert.Subject.CommonName != cn {
		t.Fatalf("CN=%q, want %q", cert.Subject.CommonName, cn)
	}
	want := x509.ExtKeyUsageClientAuth
	if server {
		want = x509.ExtKeyUsageServerAuth
	}
	found := false
	for _, u := range cert.ExtKeyUsage {
		if u == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing ExtKeyUsage %v in %v", want, cert.ExtKeyUsage)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
}
