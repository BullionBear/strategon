package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestListStrategySlotsJoinsAssigned(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 5})
	st.SetAssignment("m1", "live", &pb.StrategyAssignmentSpec{
		Strategy: "live",
		Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"},
	})
	if err := st.ApplyStatus("m1", &pb.StatusReport{
		Slots: &pb.MachineSlotStatus{Slots: []*pb.StrategySlotStatus{
			{Strategy: "live", SizeBytes: 10, CurrentVersion: "v1"},
			{Strategy: "probe-fail", SizeBytes: 435},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.ListStrategySlots(ctx, connect.NewRequest(&pb.ListStrategySlotsRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetTotalBytes() != 445 || len(resp.Msg.GetSlots()) != 2 {
		t.Fatalf("list = %+v", resp.Msg)
	}
	byName := map[string]*pb.StrategySlotView{}
	for _, s := range resp.Msg.GetSlots() {
		byName[s.GetStrategy()] = s
	}
	if !byName["live"].GetAssigned() || byName["probe-fail"].GetAssigned() {
		t.Fatalf("assigned join: %+v", byName)
	}
}

func TestListStrategySlotsEmptyWhenUnreported(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	resp, err := client.ListStrategySlots(context.Background(), connect.NewRequest(&pb.ListStrategySlotsRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.GetSlots()) != 0 {
		t.Fatalf("want empty, got %+v", resp.Msg)
	}
}

func TestReapStrategiesGates(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()

	_, err := client.ReapStrategies(ctx, connect.NewRequest(&pb.ReapStrategiesRequest{}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty machine: %v", err)
	}
	_, err = client.ReapStrategies(ctx, connect.NewRequest(&pb.ReapStrategiesRequest{MachineId: "m1"}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty names: %v", err)
	}
	_, err = client.ReapStrategies(ctx, connect.NewRequest(&pb.ReapStrategiesRequest{
		MachineId: "nope", Strategies: []string{"s"},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown machine: %v", err)
	}

	st.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 4})
	_, err = client.ReapStrategies(ctx, connect.NewRequest(&pb.ReapStrategiesRequest{
		MachineId: "m1", Strategies: []string{"s"},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want agent_version>=5, got %v", err)
	}

	st.UpsertMachine(&pb.Register{MachineId: "m2", AgentVersion: 5})
	st.SetReachable("m2", false)
	_, err = client.ReapStrategies(ctx, connect.NewRequest(&pb.ReapStrategiesRequest{
		MachineId: "m2", Strategies: []string{"s"},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want unreachable, got %v", err)
	}
}
