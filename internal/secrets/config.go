package secrets

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const (
	envKey     = "STRATEGON_SEAL_KEY"
	envKeyID   = "STRATEGON_SEAL_KEY_ID"
	envPrevKey = "STRATEGON_SEAL_PREV_KEY"
	envPrevID  = "STRATEGON_SEAL_PREV_KEY_ID"
)

// Config is the wrap-key material. Empty Key leaves SecretManagement dark.
type Config struct {
	Key     []byte
	KeyID   string
	PrevKey []byte
	PrevID  string
}

// ConfigFromEnv reads wrap keys from the process environment. An empty
// STRATEGON_SEAL_KEY is not an error (module stays dark). A present but
// unparseable value is an error so a typo cannot silently go dark.
func ConfigFromEnv() (Config, error) {
	key, err := parseKey(os.Getenv(envKey))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", envKey, err)
	}
	prev, err := parseKey(os.Getenv(envPrevKey))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", envPrevKey, err)
	}
	id := strings.TrimSpace(os.Getenv(envKeyID))
	if id == "" && len(key) > 0 {
		id = defaultKeyID(key)
	}
	prevID := strings.TrimSpace(os.Getenv(envPrevID))
	if prevID == "" && len(prev) > 0 {
		prevID = defaultKeyID(prev)
	}
	return Config{Key: key, KeyID: id, PrevKey: prev, PrevID: prevID}, nil
}

func defaultKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

func parseKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, dec := range decoders {
		b, err := dec(s)
		if err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, fmt.Errorf("must be 32 bytes as standard/url base64 or hex")
}
