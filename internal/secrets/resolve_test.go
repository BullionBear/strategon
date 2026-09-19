package secrets

import (
	"context"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestResolveDesiredStateFailClosed(t *testing.T) {
	m, _ := testModule(t)
	ctx := context.Background()
	if _, _, err := m.Put(ctx, "ok", "plain"); err != nil {
		t.Fatal(err)
	}
	ds := &pb.DesiredState{Assignments: []*pb.StrategyAssignmentSpec{{
		Strategy: "s",
		Env:      map[string]string{"A": "secret.ok", "B": "secret.missing"},
	}}}
	if err := ResolveDesiredState(ctx, m, ds); err == nil {
		t.Fatal("mixed resolve must fail")
	}
	if ds.GetAssignments()[0].GetEnv()["A"] != "secret.ok" {
		t.Fatal("failed resolve must not publish a mixed map")
	}

	ok := &pb.DesiredState{Assignments: []*pb.StrategyAssignmentSpec{{
		Strategy: "s",
		Env:      map[string]string{"A": "secret.ok", "B": "literal"},
	}}}
	if err := ResolveDesiredState(ctx, m, ok); err != nil {
		t.Fatal(err)
	}
	if ok.GetAssignments()[0].GetEnv()["A"] != "plain" || ok.GetAssignments()[0].GetEnv()["B"] != "literal" {
		t.Fatalf("resolved = %#v", ok.GetAssignments()[0].GetEnv())
	}
}

func TestResolveDesiredStateNoRefsWhileDark(t *testing.T) {
	m, err := New(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ds := &pb.DesiredState{Assignments: []*pb.StrategyAssignmentSpec{{
		Strategy: "s",
		Env:      map[string]string{"A": "plain"},
	}}}
	if err := ResolveDesiredState(context.Background(), m, ds); err != nil {
		t.Fatal(err)
	}
	ds.GetAssignments()[0].Env["A"] = "secret.x"
	if err := ResolveDesiredState(context.Background(), m, ds); err == nil {
		t.Fatal("ref while dark must fail")
	}
}
