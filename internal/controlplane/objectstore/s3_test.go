package objectstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewRequiresEndpointAndKeys(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty config")
	}
	if _, err := New(Config{Endpoint: "http://127.0.0.1:8333"}); err == nil {
		t.Fatal("expected error without keys")
	}
	s, err := New(Config{
		Endpoint:  "http://127.0.0.1:8333",
		AccessKey: "ak",
		SecretKey: "sk",
		Bucket:    "artifacts",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Bucket() != "artifacts" {
		t.Fatalf("bucket = %q", s.Bucket())
	}
}

func TestPresignGetLocalSignature(t *testing.T) {
	s, err := New(Config{
		Endpoint:  "http://127.0.0.1:8333",
		AccessKey: "ak",
		SecretKey: "sk",
		Bucket:    "artifacts",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	url, exp, err := s.PresignGet(context.Background(), "artifacts", "name/v1/abcd", DefaultPresignTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "127.0.0.1:8333") || !strings.Contains(url, "name/v1/abcd") {
		t.Fatalf("url = %q", url)
	}
	if time.Until(exp) < 4*time.Minute || time.Until(exp) > 5*time.Minute {
		t.Fatalf("expires_at = %v, want ~5m from now", exp)
	}
}
