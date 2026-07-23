// Package ingest implements registration-time artifact download into the
// control plane's S3 object store. Credentials stay CP-local (config + env);
// they never enter Postgres or any RPC response.
package ingest

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// CredType identifies how a host authenticates artifact downloads.
type CredType string

const (
	CredBearer CredType = "bearer"
	CredBasic  CredType = "basic"
	CredHeader CredType = "header"
)

// HostCredential is a resolved per-host credential (values already read from env).
type HostCredential struct {
	Type     CredType
	Token    string // bearer
	Username string // basic
	Password string // basic
	Header   string // header name
	Value    string // header value
}

// Credentials is an in-memory per-host credential map.
type Credentials struct {
	byHost map[string]HostCredential
}

type credentialsFile struct {
	ArtifactCredentials map[string]credEntry `yaml:"artifact_credentials"`
}

type credEntry struct {
	Type         string `yaml:"type"`
	TokenEnv     string `yaml:"token_env"`
	UsernameEnv  string `yaml:"username_env"`
	PasswordEnv  string `yaml:"password_env"`
	Name         string `yaml:"name"`
	ValueEnv     string `yaml:"value_env"`
}

// LoadCredentials reads a credentials.yaml file. An empty path yields an empty store.
func LoadCredentials(path string) (*Credentials, error) {
	c := &Credentials{byHost: map[string]HostCredential{}}
	if strings.TrimSpace(path) == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ingest credentials: read %s: %w", path, err)
	}
	var file credentialsFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("ingest credentials: parse %s: %w", path, err)
	}
	for host, e := range file.ArtifactCredentials {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return nil, fmt.Errorf("ingest credentials: empty host key")
		}
		cred, err := resolveCredEntry(host, e)
		if err != nil {
			return nil, err
		}
		c.byHost[host] = cred
	}
	return c, nil
}

func resolveCredEntry(host string, e credEntry) (HostCredential, error) {
	typ := CredType(strings.ToLower(strings.TrimSpace(e.Type)))
	switch typ {
	case CredBearer:
		tok := os.Getenv(e.TokenEnv)
		if e.TokenEnv == "" || tok == "" {
			return HostCredential{}, fmt.Errorf("ingest credentials: host %q bearer token_env %q is empty", host, e.TokenEnv)
		}
		return HostCredential{Type: CredBearer, Token: tok}, nil
	case CredBasic:
		user := os.Getenv(e.UsernameEnv)
		pass := os.Getenv(e.PasswordEnv)
		if e.UsernameEnv == "" || e.PasswordEnv == "" || user == "" {
			return HostCredential{}, fmt.Errorf("ingest credentials: host %q basic username_env/password_env incomplete", host)
		}
		return HostCredential{Type: CredBasic, Username: user, Password: pass}, nil
	case CredHeader:
		val := os.Getenv(e.ValueEnv)
		if e.Name == "" || e.ValueEnv == "" || val == "" {
			return HostCredential{}, fmt.Errorf("ingest credentials: host %q header name/value_env incomplete", host)
		}
		return HostCredential{Type: CredHeader, Header: e.Name, Value: val}, nil
	default:
		return HostCredential{}, fmt.Errorf("ingest credentials: host %q unknown type %q", host, e.Type)
	}
}

// Lookup returns credentials for the URI's host, if any.
func (c *Credentials) Lookup(rawURL string) (HostCredential, bool) {
	if c == nil {
		return HostCredential{}, false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return HostCredential{}, false
	}
	host := strings.ToLower(u.Hostname())
	cred, ok := c.byHost[host]
	return cred, ok
}

// HasHost reports whether any credential is configured for host.
func (c *Credentials) HasHost(host string) bool {
	if c == nil {
		return false
	}
	_, ok := c.byHost[strings.ToLower(strings.TrimSpace(host))]
	return ok
}
