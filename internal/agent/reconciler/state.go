package reconciler

import (
	"context"
	"time"

	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/supervisor"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// strategyState is the per-strategy actual state. It is read/written ONLY by
// the reconciler goroutine (RECONCILER.md §0 invariant 1: single writer).
type strategyState struct {
	strategy string
	phase    pb.DeployPhase

	runningArtifact *pb.ArtifactRef // actually running version (may lag desired)
	runningConfig   *pb.ArtifactRef
	prevArtifact    *pb.ArtifactRef // version running before the in-flight deploy (for O(1) rollback)
	prevConfig      *pb.ArtifactRef

	proc *driver.Process // nil = no process running

	inflight *deployOp // non-nil = a deploy worker is in progress; blocks concurrent deploys

	conditions map[string]*pb.Condition // Live / Ready / BusinessHealthy

	restartCount int32
	backoff      supervisor.BackoffState
	lastBadVersion string // version marked bad by auto-rollback; skipped on reconcile

	observedGen int64

	stopping     bool      // draining/retiring in progress: process exits are expected
	probeInflight bool     // a health probe goroutine is running
	healthDeadline time.Time // HEALTH_CHECKING observation window end
	startedAt    time.Time
	lastError    string
}

// deployOp tracks an in-flight deploy so it can be cancelled if desired changes.
type deployOp struct {
	target *pb.ArtifactRef
	config *pb.ArtifactRef
	cancel context.CancelFunc
}

// processExit associates a driver exit notification with its strategy.
type processExit struct {
	strategy string
	info     driver.ExitInfo
}

// workerEvent is emitted by a deploy worker to advance the phase in the main
// loop. The main loop is the sole mutator of state; the worker only performs IO.
type workerEvent struct {
	strategy string
	phase    pb.DeployPhase
	err      error
	proc     *driver.Process // STARTING/HEALTH_CHECKING success brings back the new handle
	artifact *pb.ArtifactRef
	config   *pb.ArtifactRef
}

// healthResult is fed back from an async readiness probe goroutine.
type healthResult struct {
	strategy string
	status   pb.ConditionStatus
	reason   string
	message  string
}

func newStrategyState(name string) *strategyState {
	return &strategyState{
		strategy:   name,
		phase:      pb.DeployPhase_DEPLOY_PHASE_PENDING,
		conditions: map[string]*pb.Condition{},
	}
}
