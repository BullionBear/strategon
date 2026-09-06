package artifact

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func extractFlattened(ctx context.Context, r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()

	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		rel, err := safeTarName(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "." || isWhiteout(filepath.Base(rel)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllRoot(root, dest, rel, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := mkdirAllRoot(root, dest, path.Dir(rel), 0o755); err != nil {
				return err
			}
			mode := hdr.FileInfo().Mode() & 0o777
			if err := writeFileRoot(root, rel, tr, mode); err != nil {
				return fmt.Errorf("extract %s: %w", rel, err)
			}
		case tar.TypeSymlink:
			if err := checkSymlinkTarget(rel, hdr.Linkname); err != nil {
				return err
			}
			if err := mkdirAllRoot(root, dest, path.Dir(rel), 0o755); err != nil {
				return err
			}
			target := filepath.Join(dest, filepath.FromSlash(rel))
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", rel, err)
			}
		case tar.TypeLink:
			if err := mkdirAllRoot(root, dest, path.Dir(rel), 0o755); err != nil {
				return err
			}
			srcRel, err := safeTarName(hdr.Linkname)
			if err != nil {
				return fmt.Errorf("hardlink %s: %w", rel, err)
			}
			if err := copyFileRoot(root, srcRel, rel, 0o644); err != nil {
				return fmt.Errorf("hardlink %s: %w", rel, err)
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		default:
			continue
		}
	}
}

func safeTarName(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	name = strings.ReplaceAll(name, `\`, `/`)
	if name == "" || name == "." {
		return ".", nil
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		name = strings.TrimPrefix(name, "/")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("tar slip: %q", name)
		}
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("tar slip: %q", name)
	}
	return cleaned, nil
}

func isWhiteout(base string) bool {
	return strings.HasPrefix(base, ".wh.")
}

func checkSymlinkTarget(rel, linkname string) error {
	if path.IsAbs(linkname) {
		return nil // absolute inside the container after pivot; not a host escape
	}
	dir := path.Dir(rel)
	joined := path.Clean(path.Join(dir, linkname))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return fmt.Errorf("symlink escapes rootfs: %s -> %s", rel, linkname)
	}
	return nil
}

func mkdirAllRoot(root *os.Root, dest, rel string, perm os.FileMode) error {
	if rel == "" || rel == "." {
		return nil
	}
	if err := root.MkdirAll(rel, perm); err != nil {
		// Fallback if MkdirAll is unavailable: use dest join after safeTarName.
		if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(rel)), perm); err != nil {
			return err
		}
	}
	return nil
}

func writeFileRoot(root *os.Root, rel string, r io.Reader, mode os.FileMode) error {
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Chmod(mode)
}

func copyFileRoot(root *os.Root, srcRel, dstRel string, mode os.FileMode) error {
	src, err := root.Open(srcRel)
	if err != nil {
		return err
	}
	defer src.Close()
	return writeFileRoot(root, dstRel, src, mode)
}
