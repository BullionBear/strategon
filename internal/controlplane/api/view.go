package api

import (
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/controlplane/view"
)

// BuildMachine is a thin wrapper around view.BuildMachine so existing api
// callers (ListMachines / GetMachine / WatchMachine) stay unchanged.
func BuildMachine(rec *store.MachineRecord, st store.Store) *pb.Machine {
	return view.BuildMachine(rec, st)
}

func isConverged(v *pb.StrategyView) bool { return view.IsConverged(v) }

func assignmentLive(v *pb.StrategyView) bool { return view.AssignmentLive(v) }
