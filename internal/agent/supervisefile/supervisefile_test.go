package supervisefile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	in := &File{
		AgentVersion: 3,
		Strategies: map[string]Strategy{
			"s": {
				PID: 42, StartTime: 99, PGID: 42,
				StartedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
				Phase:     "DEPLOY_PHASE_HEALTHY",
				RunningArtifact: &Artifact{
					Name: "s", Version: "v1", Digest: "sha256:aaa", URI: "file:///tmp/s",
				},
				ObservedGeneration: 7,
				LastBadVersion:     "v0",
			},
		},
	}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil || out == nil {
		t.Fatalf("load: %v %+v", err, out)
	}
	st := out.Strategies["s"]
	if st.PID != 42 || st.StartTime != 99 || st.Phase != "DEPLOY_PHASE_HEALTHY" {
		t.Fatalf("strategy: %+v", st)
	}
	if st.RunningArtifact == nil || st.RunningArtifact.Digest != "sha256:aaa" {
		t.Fatalf("artifact: %+v", st.RunningArtifact)
	}
	if out.AgentVersion != 3 || st.ObservedGeneration != 7 {
		t.Fatalf("meta: agent=%d obs=%d", out.AgentVersion, st.ObservedGeneration)
	}
}

func TestLoadMissing(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "agent", "supervision.json"))
	if err != nil || f != nil {
		t.Fatalf("want nil,nil got %+v %v", f, err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected corrupt error")
	}
}

func TestLoadUnsupportedSchema(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":99,"strategies":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected schema error")
	}
}
