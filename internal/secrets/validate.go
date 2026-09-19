package secrets

import (
	"context"
	"fmt"
	"strings"
)

// ValidateMaps rejects unknown or broken secret.<name> values. Maps with no
// refs succeed even when the module is dark. A ref while dark or missing
// fails so apply cannot persist a token it cannot later resolve.
func ValidateMaps(ctx context.Context, m *Module, maps ...map[string]string) error {
	for _, mp := range maps {
		for k, v := range mp {
			name, isRef, err := ParseRef(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			if !isRef {
				continue
			}
			if err := m.Exists(ctx, name); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	return nil
}

// MapHasRefs reports whether any value is a secret. prefix (valid or not).
func MapHasRefs(env map[string]string) bool {
	for _, v := range env {
		if strings.HasPrefix(v, Prefix) {
			return true
		}
	}
	return false
}
