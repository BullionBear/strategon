package secrets

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	// Prefix is the reserved public-token prefix. A value that starts with
	// this is a Secret reference, never a literal.
	Prefix = "secret."
	// MaxName is the longest allowed Secret name (the part after "secret.").
	MaxName = 63
)

// Token returns the public wire form "secret.<name>".
func Token(name string) string { return Prefix + name }

// ParseRef reports whether value is a Secret reference.
// isRef is false for ordinary text (including empty). A value that starts
// with "secret." but is not a valid token returns an error.
func ParseRef(value string) (name string, isRef bool, err error) {
	if !strings.HasPrefix(value, Prefix) {
		return "", false, nil
	}
	name = strings.TrimPrefix(value, Prefix)
	if err := ValidateName(name); err != nil {
		return "", true, fmt.Errorf("%w: %s", ErrBadRef, err)
	}
	return name, true, nil
}

// ValidateName checks a Secret name (the part after "secret.").
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("secret name is empty")
	}
	if len(name) > MaxName {
		return fmt.Errorf("secret name %q is longer than %d characters", name, MaxName)
	}
	if strings.HasPrefix(name, Prefix) {
		return fmt.Errorf("secret name %q must not include the %q prefix", name, Prefix)
	}
	for i, r := range name {
		if unicode.IsSpace(r) {
			return fmt.Errorf("secret name %q contains whitespace", name)
		}
		if i == 0 {
			if !isNameStart(r) {
				return fmt.Errorf("secret name %q must start with a letter or digit", name)
			}
			continue
		}
		if !isNameRune(r) {
			return fmt.Errorf("secret name %q has invalid character %q", name, r)
		}
	}
	return nil
}

func isNameStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isNameRune(r rune) bool {
	return isNameStart(r) || r == '-' || r == '_' || r == '.'
}
