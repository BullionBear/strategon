//go:build !linux

package driver

import "os"

// UserNSAvailable is always false off Linux.
func UserNSAvailable() bool { return false }

// MaybeRunOCIHelper reports whether argv requested an OCI helper. Off Linux
// probe is a no-op success; init fails so a stray re-exec does not continue
// as the agent.
func MaybeRunOCIHelper() bool {
	if len(os.Args) < 2 {
		return false
	}
	switch os.Args[1] {
	case flagOCIProbe:
		os.Exit(0)
		return true
	case flagOCIInit:
		os.Exit(2)
		return true
	}
	return false
}
