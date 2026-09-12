package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// StdioDirName is the slot-root directory for captured payload stdio.
	StdioDirName = ".stdio"
	// PayloadLogName is the current (unrotated) capture file.
	PayloadLogName = "payload.log"
	// PayloadLogMaxBytes is the size of one capture file before rotate.
	PayloadLogMaxBytes = 8 << 20
	// PayloadLogArchives is the number of rotated files kept (payload.log.1..N).
	PayloadLogArchives = 3

	stdioDirMode    = 0o750
	payloadLogMode  = 0o640
	stdioDrainAfter = 500 * time.Millisecond
)

// PayloadLogDir is <strategyDir>/.stdio. Callers must pass StrategyDir, not WorkDir.
func PayloadLogDir(strategyDir string) string {
	return filepath.Join(strategyDir, StdioDirName)
}

// PayloadLogPath is <strategyDir>/.stdio/payload.log.
func PayloadLogPath(strategyDir string) string {
	return filepath.Join(PayloadLogDir(strategyDir), PayloadLogName)
}

// StdioStartMarker is the one-line run separator written on each file open.
func StdioStartMarker(pid int, at time.Time, version string) string {
	if version == "" {
		version = "-"
	}
	return fmt.Sprintf("=== start pid=%d at=%s version=%s ===\n", pid, at.UTC().Format(time.RFC3339), version)
}

// SizeRotator appends to a file and rotates by size. Write errors degrade to
// discard so a full disk cannot stall the payload on a full pipe.
type SizeRotator struct {
	dir      string
	name     string
	maxBytes int64
	archives int
	marker   string

	f       *os.File
	size    int64
	discard bool
}

// OpenSizeRotator creates dir if needed and opens name for append.
func OpenSizeRotator(dir, name string, maxBytes int64, archives int) (*SizeRotator, error) {
	if maxBytes <= 0 {
		maxBytes = PayloadLogMaxBytes
	}
	if archives < 0 {
		archives = 0
	}
	if err := os.MkdirAll(dir, stdioDirMode); err != nil {
		return nil, err
	}
	r := &SizeRotator{dir: dir, name: name, maxBytes: maxBytes, archives: archives}
	if err := r.openCurrent(false); err != nil {
		return nil, err
	}
	return r, nil
}

// SetMarker records the start line written on this open and after every rotate.
func (r *SizeRotator) SetMarker(s string) {
	r.marker = s
	if !r.discard {
		r.writeMarker()
	}
}

func (r *SizeRotator) currentPath() string {
	return filepath.Join(r.dir, r.name)
}

func (r *SizeRotator) numberedPath(n int) string {
	if n <= 0 {
		return r.currentPath()
	}
	return r.currentPath() + fmt.Sprintf(".%d", n)
}

func (r *SizeRotator) openCurrent(truncate bool) error {
	flag := os.O_CREATE | os.O_WRONLY
	if truncate {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_APPEND
	}
	f, err := os.OpenFile(r.currentPath(), flag, payloadLogMode)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f = f
	r.size = st.Size()
	return nil
}

func (r *SizeRotator) writeMarker() {
	if r.marker == "" || r.f == nil {
		return
	}
	n, err := r.f.WriteString(r.marker)
	if err != nil {
		r.degrade()
		return
	}
	r.size += int64(n)
}

func (r *SizeRotator) degrade() {
	r.discard = true
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

// Write implements io.Writer. On rotate or disk error it never returns an
// error to the copier (discard instead).
func (r *SizeRotator) Write(p []byte) (int, error) {
	if r.discard || len(p) == 0 {
		return len(p), nil
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			r.degrade()
			return len(p), nil
		}
	}
	if r.f == nil {
		return len(p), nil
	}
	n, err := r.f.Write(p)
	if err != nil {
		r.degrade()
		return len(p), nil
	}
	r.size += int64(n)
	if r.size >= r.maxBytes {
		if err := r.rotate(); err != nil {
			r.degrade()
		}
	}
	return len(p), nil
}

func (r *SizeRotator) rotate() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	if r.archives > 0 {
		_ = os.Remove(r.numberedPath(r.archives))
		for i := r.archives; i >= 1; i-- {
			src := r.numberedPath(i - 1)
			dst := r.numberedPath(i)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	if err := r.openCurrent(true); err != nil {
		return err
	}
	r.writeMarker()
	return nil
}

// Close flushes the current file.
func (r *SizeRotator) Close() error {
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
