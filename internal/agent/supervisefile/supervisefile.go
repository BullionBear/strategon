// Package supervisefile persists and loads the agent's supervision snapshot
// used for self-update / restart takeover (RECONCILER.md §10, IMPROVEMENT B5).
package supervisefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is the only supported on-disk format for now.
const SchemaVersion = 1

// RelativePath is under the agent --base directory.
const RelativePath = "agent/supervision.json"

// File is the top-level supervision snapshot.
type File struct {
	SchemaVersion int                 `json:"schema_version"`
	WrittenAt     time.Time           `json:"written_at"`
	AgentVersion  int                 `json:"agent_version"`
	Strategies    map[string]Strategy `json:"strategies"`
}

// Strategy is one supervised process entry.
type Strategy struct {
	PID                int       `json:"pid"`
	StartTime          uint64    `json:"start_time"`
	PGID               int       `json:"pgid"`
	StartedAt          time.Time `json:"started_at"`
	Phase              string    `json:"phase"`
	RunningArtifact    *Artifact `json:"running_artifact,omitempty"`
	RunningConfig      *Artifact `json:"running_config,omitempty"`
	ObservedGeneration int64     `json:"observed_generation"`
	LastBadVersion     string    `json:"last_bad_version,omitempty"`
}

// Artifact is a JSON-friendly ArtifactRef subset.
type Artifact struct {
	Type    string `json:"type,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	URI     string `json:"uri,omitempty"`
}

// Path returns <--base>/agent/supervision.json.
func Path(baseDir string) string {
	return filepath.Join(baseDir, RelativePath)
}

// Load reads the supervision file. Missing file returns (nil, nil).
// Corrupt JSON or unsupported schema_version returns an error.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("supervisefile: corrupt: %w", err)
	}
	if f.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("supervisefile: unsupported schema_version %d (want %d)", f.SchemaVersion, SchemaVersion)
	}
	if f.Strategies == nil {
		f.Strategies = map[string]Strategy{}
	}
	return &f, nil
}

// Save atomically writes the supervision file (tmp → fsync → rename).
func Save(path string, f *File) error {
	if f == nil {
		return errors.New("supervisefile: nil file")
	}
	f.SchemaVersion = SchemaVersion
	f.WrittenAt = time.Now().UTC()
	if f.Strategies == nil {
		f.Strategies = map[string]Strategy{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := fh.Write(b); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
