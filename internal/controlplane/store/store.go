// Package store is the control plane's state boundary: desired state (spec),
// observed state (status), machine registry, artifact catalog, and audit log.
// spec is written ONLY by the control plane; status is written ONLY by agents
// (PROTOCOL.md §0).
//
// The interface keeps the backing store swappable; the v1 implementation is
// in-memory (Postgres/sqlc is a deferred follow-up, ARCHITECTURE.md §16.3).
// Every spec mutation bumps a per-machine monotonically increasing generation,
// the sole coupling between desired and observed.
package store

import (
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// MachineRecord is the control plane's view of a machine.
type MachineRecord struct {
	MachineID         string
	Register          *pb.Register
	Reachable         bool
	AgentVersion      int32  // capability version (monotonic)
	AgentBuildVersion string // buildinfo.Version — display only
	LastResources     *pb.MachineResources
	LastHeartbeat     int64 // unix seconds; 0 = never
	Generation        int64
	Assignments       map[string]*pb.StrategyAssignmentSpec // strategy -> spec
	Status            map[string]*pb.StrategyAssignmentStatus
	// PreviousArtifacts tracks the last replaced artifact per strategy so
	// Rollback with empty target_version can re-point desired state (FRONTEND.md §2.2).
	PreviousArtifacts map[string]*pb.ArtifactRef
	ObservedGen       int64
}

// Store is the control-plane persistence boundary.
type Store interface {
	// UpsertMachine registers or updates a machine on (re)connect. Returns the
	// current record.
	UpsertMachine(reg *pb.Register) (*MachineRecord, error)

	// GetMachine returns a snapshot copy of the machine record.
	GetMachine(machineID string) (*MachineRecord, bool)

	// ListMachines returns snapshot copies of all machine records.
	ListMachines() []*MachineRecord

	// DesiredState builds the current full DesiredState snapshot for a machine.
	DesiredState(machineID string) (*pb.DesiredState, bool)

	// SetAssignment sets (or, with nil spec, removes) a strategy assignment and
	// bumps the machine generation. Returns the new generation.
	SetAssignment(machineID, strategy string, spec *pb.StrategyAssignmentSpec) (int64, error)

	// ApplyStatus records an agent-reported StatusReport.
	ApplyStatus(machineID string, report *pb.StatusReport) error

	// ApplyHeartbeat records a heartbeat (resources, observed generation, agent versions).
	ApplyHeartbeat(machineID string, hb *pb.Heartbeat, atUnix int64) error

	// SetReachable marks a machine reachable/unreachable.
	SetReachable(machineID string, reachable bool) error

	// AppendAudit records an audit entry (deploy/rollback/config change).
	AppendAudit(entry *pb.AuditEntry) error

	// ListAudit returns audit entries, newest first (optionally filtered).
	ListAudit(machineID, strategy string) []*pb.AuditEntry

	// RegisterArtifact upserts an artifact into the catalog (keyed by name+version).
	RegisterArtifact(ref *pb.ArtifactRef) error

	// GetArtifact looks up a registered artifact by name and version.
	GetArtifact(name, version string) (*pb.ArtifactRef, bool)

	// ListArtifacts returns registered artifacts, optionally filtered by name.
	ListArtifacts(name string) []*pb.ArtifactRef

	// PreviousArtifact returns the artifact that was replaced by the last Deploy
	// for the given machine/strategy (for empty-target Rollback).
	PreviousArtifact(machineID, strategy string) (*pb.ArtifactRef, bool)

	// AcquireLease grants or refreshes a fencing lease for strategy on machineID
	// (PROTOCOL §10.4). Denied when another machine holds an unexpired lease
	// (including margin_cp).
	AcquireLease(machineID, strategy string, ttl time.Duration) (LeaseResult, error)

	// RenewLease extends a lease; only the current holder with matching lease_id.
	RenewLease(machineID, strategy, leaseID string, ttl time.Duration) (LeaseResult, error)

	// GetLease returns the current lease record for strategy, if any.
	GetLease(strategy string) (LeaseInfo, bool)

	// LeaseMarginCP returns the control-plane expiry margin (SAFETY §2).
	LeaseMarginCP() time.Duration
}
