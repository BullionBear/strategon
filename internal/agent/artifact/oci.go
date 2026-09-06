package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

const (
	ociMetaName  = "oci.json"
	ociTarName   = "image.tar"
	ociRootfsDir = "rootfs"
	ociSchema    = 1
)

// OCIMeta is the on-disk marker for an unpacked OCI release. Digest is the
// sha256 of the original archive (not the image config digest).
type OCIMeta struct {
	SchemaVersion int      `json:"schema_version"`
	Digest        string   `json:"digest"`
	Entrypoint    []string `json:"entrypoint"`
	Cmd           []string `json:"cmd"`
	Env           []string `json:"env"`
	WorkingDir    string   `json:"working_dir"`
	User          string   `json:"user"`
	StopSignal    string   `json:"stop_signal"`
	Platform      string   `json:"platform"`
}

func isOCI(ref *pb.ArtifactRef) bool {
	return ref != nil && ref.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE
}

// ImageTarPath is releases/<ver>/image.tar (transient during Download).
func (m *Manager) ImageTarPath(strategy, version string) string {
	return filepath.Join(m.ReleaseDir(strategy, version), ociTarName)
}

// RootfsPath is releases/<ver>/rootfs.
func (m *Manager) RootfsPath(strategy, version string) string {
	return filepath.Join(m.ReleaseDir(strategy, version), ociRootfsDir)
}

// OCIMetaPath is releases/<ver>/oci.json.
func (m *Manager) OCIMetaPath(strategy, version string) string {
	return filepath.Join(m.ReleaseDir(strategy, version), ociMetaName)
}

// CurrentRootfsPath resolves rootfs via the current symlink.
func (m *Manager) CurrentRootfsPath(strategy string) string {
	return filepath.Join(m.CurrentLink(strategy), ociRootfsDir)
}

// CurrentOCIMetaPath resolves oci.json via the current symlink.
func (m *Manager) CurrentOCIMetaPath(strategy string) string {
	return filepath.Join(m.CurrentLink(strategy), ociMetaName)
}

// WorkDir is <base>/<strategy>/work — OCI cwd and bind target.
func (m *Manager) WorkDir(strategy string) string {
	return filepath.Join(m.StrategyDir(strategy), "work")
}

func (m *Manager) readOCIMeta(strategy, version string) (*OCIMeta, error) {
	return readOCIMetaFile(m.OCIMetaPath(strategy, version))
}

// ReadCurrentOCIMeta loads oci.json through the current symlink.
func ReadCurrentOCIMeta(m *Manager, strategy string) (*OCIMeta, error) {
	return readOCIMetaFile(m.CurrentOCIMetaPath(strategy))
}

func readOCIMetaFile(path string) (*OCIMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta OCIMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("oci.json: %w", err)
	}
	if meta.SchemaVersion != ociSchema {
		return nil, fmt.Errorf("oci.json: unsupported schema_version %d", meta.SchemaVersion)
	}
	return &meta, nil
}

func writeOCIMeta(path string, meta *OCIMeta) error {
	meta.SchemaVersion = ociSchema
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ParseUser maps an image Config.User to a single uid/gid pair. Names are
// not resolved (v1); they fall back to 0. A bare uid uses gid = uid.
func ParseUser(s string) (uid, gid int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0
	}
	parts := strings.SplitN(s, ":", 2)
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0
	}
	if len(parts) == 1 || parts[1] == "" {
		return uid, uid
	}
	gid, err = strconv.Atoi(parts[1])
	if err != nil {
		return uid, uid
	}
	return uid, gid
}

// EnsureWorkDir creates the OCI work directory.
func (m *Manager) EnsureWorkDir(strategy string) (string, error) {
	dir := m.WorkDir(strategy)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir work: %w", err)
	}
	return dir, nil
}
