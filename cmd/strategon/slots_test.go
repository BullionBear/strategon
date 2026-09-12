package main

import (
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestReapExitError(t *testing.T) {
	if err := reapExitError([]*pb.ReapStrategyResult{{Strategy: "s", Removed: true}}); err != nil {
		t.Fatalf("all removed: %v", err)
	}
	if err := reapExitError(nil); err == nil {
		t.Fatal("empty results should fail")
	}
	if err := reapExitError([]*pb.ReapStrategyResult{{Strategy: "s", Error: "still assigned"}}); err == nil {
		t.Fatal("per-name failure should fail")
	}
	if err := reapExitError([]*pb.ReapStrategyResult{
		{Strategy: "a", Removed: true},
		{Strategy: "b", Error: "process still running"},
	}); err == nil {
		t.Fatal("mixed results should fail")
	}
}
