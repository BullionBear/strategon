// Package sharedfile holds validation and helpers for machine-scoped shared
// files used by both the control-plane API and the agent.
package sharedfile

import (
	"fmt"
	"strings"
)

// ReservedName is the on-disk store directory under <base>/shared; it cannot
// be used as a shared-file basename.
const ReservedName = "store"

// ValidateName rejects empty names, path separators, "." / ".." components,
// NUL bytes, and the reserved name "store". Valid names are clean basenames
// written under <base>/shared/<name>.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("shared file name is empty")
	}
	if name == ReservedName {
		return fmt.Errorf("shared file name %q is reserved", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("shared file name contains NUL")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("shared file name must be a basename (no path separators)")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("shared file name %q is invalid", name)
	}
	return nil
}
