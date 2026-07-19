package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPeerCNHandler(t *testing.T) {
	var got string
	var ok bool
	h := PeerCNHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = PeerCN(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if ok || got != "" {
		t.Fatalf("plaintext PeerCN=(%q,%v), want empty", got, ok)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: "m1"},
		}},
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !ok || got != "m1" {
		t.Fatalf("mtls PeerCN=(%q,%v), want m1", got, ok)
	}
}
