package assignmentset

import (
	"strings"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func testSet(spec *pb.AssignmentSetSpec) *pb.AssignmentSet {
	if spec.Strategy == "" {
		spec.Strategy = "nats"
	}
	return &pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec:     spec,
	}
}

func TestWantConfigImplicitInheritsSetVersion(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		ConfigVersion: "c1",
		Members: []*pb.SetMember{
			{Machine: "m1", Name: "nats-m1"},
		},
	})
	got, err := WantConfig(set, 0, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != "nats-config" || got.Fallback != "nats-config" || got.Version != "c1" {
		t.Fatalf("got %+v", got)
	}
}

func TestWantConfigNoVersionOmitsConfig(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Members: []*pb.SetMember{{Machine: "m1", Name: "nats-m1"}},
	})
	got, err := WantConfig(set, 0, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if got != (ConfigWant{}) {
		t.Fatalf("got %+v, want empty", got)
	}
}

func TestWantConfigMemberOverridesName(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		ConfigVersion: "c1",
		Members: []*pb.SetMember{
			{Machine: "m1", Name: "nats-m1"},
			{Machine: "m2", Name: "nats-m2", Config: "nats-m2-config"},
		},
	})
	a, err := WantConfig(set, 0, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if a.Primary != "nats-config" || a.Fallback == "" || a.Version != "c1" {
		t.Fatalf("member0 = %+v", a)
	}
	b, err := WantConfig(set, 1, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if b.Primary != "nats-m2-config" || b.Fallback != "" || b.Version != "c1" {
		t.Fatalf("member1 = %+v", b)
	}
}

func TestWantConfigMemberOverridesVersion(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		ConfigVersion: "c1",
		Members: []*pb.SetMember{
			{Machine: "m1", Name: "nats-m1", ConfigVersion: "c2"},
		},
	})
	got, err := WantConfig(set, 0, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "c2" || got.Fallback == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestWantConfigSetNameTemplatePerMember(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Config:        "${member.name}-config",
		ConfigVersion: "c1",
		Members: []*pb.SetMember{
			{Machine: "m1", Name: "nats-m1"},
			{Machine: "m2", Name: "nats-m2"},
		},
	})
	a, err := WantConfig(set, 0, "nats")
	if err != nil {
		t.Fatal(err)
	}
	b, err := WantConfig(set, 1, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if a.Primary != "nats-m1-config" || a.Fallback != "" {
		t.Fatalf("member0 = %+v", a)
	}
	if b.Primary != "nats-m2-config" || b.Fallback != "" {
		t.Fatalf("member1 = %+v", b)
	}
}

func TestWantConfigExplicitNameNoFallback(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Config:        "custom-conf",
		ConfigVersion: "c1",
		Members:       []*pb.SetMember{{Machine: "m1", Name: "nats-m1"}},
	})
	got, err := WantConfig(set, 0, "nats")
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != "custom-conf" || got.Fallback != "" {
		t.Fatalf("got %+v, want no fallback", got)
	}
}

func TestWantConfigExplicitNameRequiresVersion(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Config:  "custom-conf",
		Members: []*pb.SetMember{{Machine: "m1", Name: "nats-m1"}},
	})
	_, err := WantConfig(set, 0, "nats")
	if err == nil || !strings.Contains(err.Error(), "requires a version") {
		t.Fatalf("err = %v", err)
	}
}

func TestWantConfigMemberNameRequiresVersion(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Members: []*pb.SetMember{{Machine: "m1", Name: "nats-m1", Config: "nats-m1-config"}},
	})
	_, err := WantConfig(set, 0, "nats")
	if err == nil || !strings.Contains(err.Error(), "requires a version") {
		t.Fatalf("err = %v", err)
	}
}

func TestWantConfigEmptyExpand(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		ConfigVersion: "c1",
		Members: []*pb.SetMember{{
			Machine: "m1", Name: "nats-m1",
			Config: "${member.vars.cfg}",
			Vars:   map[string]string{"cfg": ""},
		}},
	})
	_, err := WantConfig(set, 0, "nats")
	if err == nil || !strings.Contains(err.Error(), "empty config name") {
		t.Fatalf("err = %v", err)
	}
}

func TestWantConfigUnknownPlaceholder(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Config:        "${peers}-config",
		ConfigVersion: "c1",
		Members:       []*pb.SetMember{{Machine: "m1", Name: "nats-m1"}},
	})
	_, err := WantConfig(set, 0, "nats")
	if err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Fatalf("err = %v", err)
	}
}

func TestWantConfigIndexOutOfRange(t *testing.T) {
	set := testSet(&pb.AssignmentSetSpec{
		Members: []*pb.SetMember{{Machine: "m1", Name: "nats-m1"}},
	})
	if _, err := WantConfig(set, 1, "nats"); err == nil {
		t.Fatal("expected range error")
	}
}
