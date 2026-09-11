package volume

import (
	"errors"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestValidateName(t *testing.T) {
	ok := []string{"data", "nats-a-data", "mftik.logs"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v", n, err)
		}
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "lost+found"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) want error", n)
		}
	}
}

func TestValidateMounts(t *testing.T) {
	if err := ValidateMounts([][2]string{{"data", "/var/lib/mftik"}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMounts([][2]string{{"data", "relative"}}); err == nil {
		t.Fatal("expected relative path error")
	}
	if err := ValidateMounts([][2]string{{"data", "/"}}); err == nil {
		t.Fatal("expected / rejected")
	}
	if err := ValidateMounts([][2]string{{"data", "/tmp"}}); err == nil {
		t.Fatal("expected /tmp rejected")
	}
	if err := ValidateMounts([][2]string{{"data", "/tmp/data"}}); err == nil {
		t.Fatal("expected /tmp/data rejected")
	}
	if err := ValidateMounts([][2]string{
		{"a", "/var/lib/mftik"},
		{"b", "/var/lib/mftik/sub"},
	}); err == nil {
		t.Fatal("expected overlap")
	}
}

func TestValidateContainerPathRejectsTmp(t *testing.T) {
	for _, p := range []string{"/tmp", "/tmp/", "/tmp/data", "/tmp/data/sub"} {
		if err := ValidateContainerPath(p); err == nil {
			t.Errorf("ValidateContainerPath(%q) want error", p)
		}
	}
	if err := ValidateContainerPath("/var/tmp"); err != nil {
		t.Fatalf("/var/tmp should be allowed: %v", err)
	}
}

func TestLiveWriterConflict(t *testing.T) {
	mounts := []*pb.VolumeMount{{Name: "data", ContainerPath: "/var/lib/mftik"}}
	assignments := map[string]*pb.StrategyAssignmentSpec{
		"a": {Strategy: "a", VolumeMounts: mounts},
		"stopped": {
			Strategy:     "stopped",
			Stopped:      true,
			VolumeMounts: mounts,
		},
	}
	if err := LiveWriterConflict("b", mounts, assignments); err == nil {
		t.Fatal("expected conflict with running a")
	} else {
		var wc *WriterConflictError
		if !errors.As(err, &wc) || wc.Volume != "data" || wc.Assignment != "a" {
			t.Fatalf("err = %v", err)
		}
	}
	if err := LiveWriterConflict("a", mounts, assignments); err != nil {
		t.Fatalf("self should not conflict: %v", err)
	}
	if err := LiveWriterConflict("b", []*pb.VolumeMount{{Name: "other", ContainerPath: "/x"}}, assignments); err != nil {
		t.Fatalf("different volume: %v", err)
	}
	if err := LiveWriterConflict("b", mounts, map[string]*pb.StrategyAssignmentSpec{
		"stopped": assignments["stopped"],
	}); err != nil {
		t.Fatalf("stopped assignment is not a writer: %v", err)
	}
}

func TestShadowsBindSame(t *testing.T) {
	if !ShadowsBindSame("/var/lib/strategon/sts/work", "/var/lib/strategon/sts/work") {
		t.Fatal("equal path should shadow")
	}
	if ShadowsBindSame("/var/lib/mftik", "/var/lib/strategon/sts/work") {
		t.Fatal("unrelated path should not shadow")
	}
}
