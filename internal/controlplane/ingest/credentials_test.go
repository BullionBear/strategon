package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCredentials(t *testing.T) {
	t.Setenv("TEST_BEARER", "tok-123")
	t.Setenv("TEST_BASIC_USER", "u")
	t.Setenv("TEST_BASIC_PASS", "p")
	t.Setenv("TEST_HDR", "secret")

	path := filepath.Join(t.TempDir(), "credentials.yaml")
	body := `
artifact_credentials:
  api.github.com:
    type: bearer
    token_env: TEST_BEARER
  internal.example.org:
    type: basic
    username_env: TEST_BASIC_USER
    password_env: TEST_BASIC_PASS
  registry.local:
    type: header
    name: X-API-Key
    value_env: TEST_HDR
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := creds.Lookup("https://api.github.com/foo")
	if !ok || c.Type != CredBearer || c.Token != "tok-123" {
		t.Fatalf("bearer = %+v ok=%v", c, ok)
	}
	c, ok = creds.Lookup("https://internal.example.org/a")
	if !ok || c.Type != CredBasic || c.Username != "u" || c.Password != "p" {
		t.Fatalf("basic = %+v ok=%v", c, ok)
	}
	c, ok = creds.Lookup("https://registry.local/x")
	if !ok || c.Type != CredHeader || c.Header != "X-API-Key" || c.Value != "secret" {
		t.Fatalf("header = %+v ok=%v", c, ok)
	}
}

func TestLoadCredentialsEmptyPath(t *testing.T) {
	c, err := LoadCredentials("")
	if err != nil || c == nil {
		t.Fatalf("empty path: %v %#v", err, c)
	}
	if _, ok := c.Lookup("https://example.com"); ok {
		t.Fatal("expected no credentials")
	}
}
