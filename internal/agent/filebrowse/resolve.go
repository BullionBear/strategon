package filebrowse

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Root opens a jailed filesystem root at strategyDir. Callers must Close it.
func Root(strategyDir string) (*os.Root, error) {
	if strategyDir == "" {
		return nil, fmt.Errorf("empty strategy dir")
	}
	return os.OpenRoot(strategyDir)
}

// NormalizeRel cleans a request path relative to the strategy root.
// Rejects absolute paths and any ".." component before they reach the OS.
// "" and "." both mean the root itself (returned as ".").
func NormalizeRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return ".", nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	raw := strings.ReplaceAll(p, `\`, `/`)
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes strategy root")
		}
	}
	cleaned := path.Clean(raw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes strategy root")
	}
	if cleaned == "." || cleaned == "" {
		return ".", nil
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	return cleaned, nil
}

// ValidateStrategy rejects empty names and path traversal in the strategy
// segment before it is joined into StrategyDir.
func ValidateStrategy(strategy string) error {
	strategy = strings.TrimSpace(strategy)
	if strategy == "" {
		return fmt.Errorf("empty strategy")
	}
	if strings.ContainsAny(strategy, `/\`) || strategy == ".." || strings.Contains(strategy, "..") {
		return fmt.Errorf("invalid strategy name")
	}
	if filepath.IsAbs(strategy) {
		return fmt.Errorf("invalid strategy name")
	}
	return nil
}
