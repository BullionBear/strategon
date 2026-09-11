package assign

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

type stubAgents struct{ n int }

func (s *stubAgents) Notify(string) { s.n++ }

type stubReserve struct {
	cluster string
	ok      bool
}

func (s stubReserve) ReservedBy(string, string) (string, bool) { return s.cluster, s.ok }

func setup(t *testing.T) (*Service, *store.Memory, *stubAgents) {
	t.Helper()
	st := store.NewMemory(nil)
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	agents := &stubAgents{}
	return New(st, agents), st, agents
}

func TestApplyIdenticalSpecIsNoop(t *testing.T) {
	svc, st, agents := setup(t)
	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa"},
	}
	g1, changed, err := svc.Apply(context.Background(), Request{
		MachineID: "m1", Strategy: "s", Spec: spec, Action: "Deploy", ToVersion: "v1",
	})
	if err != nil || !changed || g1 != 1 {
		t.Fatalf("first: gen=%d changed=%v err=%v", g1, changed, err)
	}
	if agents.n != 1 {
		t.Fatalf("notify=%d, want 1", agents.n)
	}
	if n := len(st.ListAudit("m1", "s")); n != 1 {
		t.Fatalf("audit=%d, want 1", n)
	}

	g2, changed, err := svc.Apply(context.Background(), Request{
		MachineID: "m1", Strategy: "s", Spec: spec, Action: "Deploy", ToVersion: "v1",
	})
	if err != nil || changed || g2 != g1 {
		t.Fatalf("noop: gen=%d changed=%v err=%v", g2, changed, err)
	}
	if agents.n != 1 {
		t.Fatalf("noop must not notify, n=%d", agents.n)
	}
	if n := len(st.ListAudit("m1", "s")); n != 1 {
		t.Fatalf("noop must not audit, n=%d", n)
	}
}

func TestApplyEnvChangeBumpsAndNotifies(t *testing.T) {
	svc, _, agents := setup(t)
	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa"},
		Env:      map[string]string{"A": "1"},
	}
	g1, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "s", Spec: spec, Action: "SetDeployment"})
	if err != nil {
		t.Fatal(err)
	}
	next := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa"},
		Env:      map[string]string{"A": "2"},
	}
	g2, changed, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "s", Spec: next, Action: "SetDeployment"})
	if err != nil || !changed || g2 <= g1 {
		t.Fatalf("env change: gen=%d changed=%v err=%v", g2, changed, err)
	}
	if agents.n != 2 {
		t.Fatalf("notify=%d, want 2", agents.n)
	}
}

func TestApplyNoopLeavesPreviousArtifacts(t *testing.T) {
	svc, st, _ := setup(t)
	v1 := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"}}
	if _, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "s", Spec: v1, Action: "Deploy"}); err != nil {
		t.Fatal(err)
	}
	v2 := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v2", Digest: "sha256:bbb"}}
	if _, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "s", Spec: v2, Action: "Deploy"}); err != nil {
		t.Fatal(err)
	}
	prev, ok := st.PreviousArtifact("m1", "s")
	if !ok || prev.GetVersion() != "v1" {
		t.Fatalf("previous=%+v ok=%v", prev, ok)
	}
	if _, changed, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "s", Spec: v2, Action: "Deploy"}); err != nil || changed {
		t.Fatalf("noop changed=%v err=%v", changed, err)
	}
	prev, ok = st.PreviousArtifact("m1", "s")
	if !ok || prev.GetVersion() != "v1" {
		t.Fatalf("noop overwrote previous: %+v", prev)
	}
}

func TestLeaseInterlockHumanVsController(t *testing.T) {
	svc, st, _ := setup(t)
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLease("m1", "s", time.Minute); err != nil {
		t.Fatal(err)
	}
	spec := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"}}
	_, _, err := svc.Apply(context.Background(), Request{
		MachineID: "m2", Strategy: "s", Spec: spec, Action: "Deploy", EnforceLeaseInterlock: true,
	})
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("human write should be blocked: %v", err)
	}
	_, changed, err := svc.Apply(context.Background(), Request{
		MachineID: "m2", Strategy: "s", Spec: spec, Action: "Reconcile", EnforceLeaseInterlock: false,
	})
	if err != nil || !changed {
		t.Fatalf("controller write should pass: changed=%v err=%v", changed, err)
	}
}

func TestReservationBlocksUnlessAllowed(t *testing.T) {
	svc, _, _ := setup(t)
	svc.Reservation = stubReserve{cluster: "trading", ok: true}
	spec := &pb.StrategyAssignmentSpec{Strategy: "nats", Artifact: &pb.ArtifactRef{Version: "v1"}}
	_, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "nats", Spec: spec, Action: "Deploy"})
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("reserved write should fail: %v", err)
	}
	_, changed, err := svc.Apply(context.Background(), Request{
		MachineID: "m1", Strategy: "nats", Spec: spec, Action: "Reconcile", AllowReserved: true,
	})
	if err != nil || !changed {
		t.Fatalf("AllowReserved should write: changed=%v err=%v", changed, err)
	}
}

func TestApplyRejectsSecondLiveVolumeWriter(t *testing.T) {
	svc, _, _ := setup(t)
	a := &pb.StrategyAssignmentSpec{
		Strategy:     "a",
		Artifact:     &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"},
		VolumeMounts: []*pb.VolumeMount{{Name: "data", ContainerPath: "/var/lib/mftik"}},
	}
	if _, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "a", Spec: a, Action: "Deploy"}); err != nil {
		t.Fatal(err)
	}
	b := &pb.StrategyAssignmentSpec{
		Strategy:     "b",
		Artifact:     &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"},
		VolumeMounts: []*pb.VolumeMount{{Name: "data", ContainerPath: "/var/lib/mftik"}},
	}
	_, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "b", Spec: b, Action: "Deploy"})
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second writer: %v", err)
	}
	b.Stopped = true
	if _, _, err := svc.Apply(context.Background(), Request{MachineID: "m1", Strategy: "b", Spec: b, Action: "Deploy"}); err != nil {
		t.Fatalf("stopped writer should apply: %v", err)
	}
}
