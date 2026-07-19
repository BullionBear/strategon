// Package buildinfo holds compile-time build metadata injected via ldflags.
// Defaults apply to bare `go build` without make; `make build` overwrites them
// with git describe / commit / UTC build time. Values are for human display
// only — never use them for compatibility checks or upgrade decisions.
package buildinfo

var (
	Version    = "dev" // git describe result, or local "dev"
	CommitHash = "unknown"
	BuildTime  = "unknown"
)
