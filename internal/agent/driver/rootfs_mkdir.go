package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mkdirAllUnderRootfs creates containerPath under rootfs without following
// any symlink. filepath.Join + MkdirAll would resolve an image symlink such
// as /var/run → /run against the host before pivot_root.
func mkdirAllUnderRootfs(rootfs, containerPath string) (string, error) {
	if containerPath == "" {
		return "", fmt.Errorf("empty container path")
	}
	cleaned := filepath.Clean(containerPath)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("container path %q must be absolute", containerPath)
	}
	rel, err := filepath.Rel("/", cleaned)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", fmt.Errorf("container path cannot be /")
	}
	if !filepath.IsAbs(rootfs) {
		rootfs, err = filepath.Abs(rootfs)
		if err != nil {
			return "", err
		}
	}
	cur := rootfs
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("container path escapes rootfs")
		}
		next := filepath.Join(cur, part)
		if escaped, err := pathEscapesRoot(rootfs, next); err != nil {
			return "", err
		} else if escaped {
			return "", fmt.Errorf("container path escapes rootfs")
		}
		st, err := os.Lstat(next)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			if err := os.Mkdir(next, 0o755); err != nil {
				return "", err
			}
			cur = next
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to follow symlink at %s", next)
		}
		if !st.IsDir() {
			return "", fmt.Errorf("%s is not a directory", next)
		}
		cur = next
	}
	return cur, nil
}

func pathEscapesRoot(root, p string) (bool, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return true, err
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)), nil
}
