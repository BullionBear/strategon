// Package store is the control plane's state boundary: desired state (spec),
// observed state (status), machine registry, artifact catalog, and audit log.
// spec is written ONLY by the control plane; status is written ONLY by agents.
//
// The interface keeps the backing store swappable; the v1 implementation is
// in-memory (Postgres/sqlc is a deferred follow-up).
// Every spec mutation bumps a per-machine monotonically increasing generation,
// the sole coupling between desired and observed.
package store

import (
	"context"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// Resource sample window defaults (short-term sparkline, not long-term history).
const (
	ResourceSampleInterval = time.Minute
	ResourceSampleRetain   = time.Hour
)

// ResourceSample is one point in the sliding window.
type ResourceSample struct {
	SampledAt  time.Time
	CPUPercent float64
	MemBytes   int64
}

// TokenRow is a persisted API token. Only the SHA-256 hash of the plaintext
// secret is stored — never the secret itself.
type TokenRow struct {
	ID        string
	TokenHash string
	Name      string
	UserID    string
	Username  string
	CreatedAt time.Time
	LastUsed  time.Time // zero if never used / unknown
	RevokedAt time.Time // zero if active (soft-delete when set)
}

// MachineRecord is the control plane's view of a machine.
type MachineRecord struct {
	MachineID         string
	Register          *pb.Register
	Reachable         bool
	AgentVersion      int32  // capability version (monotonic)
	AgentBuildVersion string // buildinfo.Version — display only
	LastResources     *pb.MachineResources
	LastProcesses     []*pb.ProcessMetrics // latest Heartbeat process snapshot
	LastHeartbeat     int64                // unix seconds; 0 = never
	Generation        int64
	Assignments       map[string]*pb.StrategyAssignmentSpec // strategy -> spec
	Status            map[string]*pb.StrategyAssignmentStatus
	// PreviousArtifacts tracks the last replaced artifact per strategy so
	// Rollback with empty target_version can re-point desired state.
	PreviousArtifacts map[string]*pb.ArtifactRef
	ObservedGen       int64
	// Machine-scoped shared files (independent of assignment generations).
	SharedGeneration  int64
	SharedFiles       map[string]*pb.SharedFileSpec // name -> spec with ArtifactRef
	SharedStatus      *pb.MachineSharedStatus       // latest agent-reported status
	VolumesGeneration int64
	Volumes           map[string]*pb.VolumeSpec // name -> spec
	VolumesStatus     *pb.MachineVolumeStatus
	SlotsStatus       *pb.MachineSlotStatus // latest agent-reported on-disk slots
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

	// SetAssignment sets (or, with nil spec, removes) a strategy assignment.
	// An identical spec (proto.Equal) is a no-op: generation is not bumped,
	// PreviousArtifacts is untouched, and notify is not fired. Removing a
	// missing assignment is also a no-op. Returns (generation, changed).
	SetAssignment(machineID, strategy string, spec *pb.StrategyAssignmentSpec) (gen int64, changed bool, err error)

	// SetSharedFiles replaces the full set of machine-level shared files.
	// When the desired set is unchanged (same names → digests), it is a no-op:
	// generations are not bumped and notify is not fired. files must carry
	// resolved ArtifactRefs (digest + uri). Returns (sharedGen, desiredGen, changed).
	SetSharedFiles(machineID string, files []*pb.SharedFileSpec) (sharedGen, desiredGen int64, changed bool, err error)

	// CreateVolume adds a named volume to the machine inventory. Identical
	// name is a no-op. Returns (volumesGen, desiredGen, changed).
	CreateVolume(machineID, name string) (volGen, desiredGen int64, changed bool, err error)

	// DeleteVolume removes a named volume from desired inventory. The caller
	// is responsible for occupancy checks. Missing name is a no-op.
	DeleteVolume(machineID, name string) (volGen, desiredGen int64, changed bool, err error)

	// ApplyStatus records an agent-reported StatusReport. The report's
	// Assignments are a full snapshot of strategies the agent still tracks;
	// statuses for strategies absent from the report are pruned (so a finished
	// undeploy/drain does not leave a DRAINING tombstone in the UI).
	// report.Shared is persisted as the machine's shared_status when present.
	// report.Volumes is persisted as volumes_status when present.
	// report.Slots is persisted as slots_status when present (nil = do not
	// overwrite; empty wrapper = walked, nothing on disk).
	ApplyStatus(machineID string, report *pb.StatusReport) error

	// ApplyHeartbeat records a heartbeat (resources, observed generation, agent versions).
	// Also appends to the short-term resource_samples window at most once per
	// ResourceSampleInterval.
	ApplyHeartbeat(machineID string, hb *pb.Heartbeat, atUnix int64) error

	// ListResourceSamples returns sliding-window samples for machine/strategy
	// (strategy "" = machine-level), oldest first, within [since, now].
	ListResourceSamples(machineID, strategy string, since time.Time) ([]ResourceSample, error)

	// SetReachable marks a machine reachable/unreachable.
	SetReachable(machineID string, reachable bool) error

	// AppendAudit records an audit entry (deploy/rollback/config change).
	AppendAudit(entry *pb.AuditEntry) error

	// ListAudit returns audit entries, newest first (optionally filtered).
	ListAudit(machineID, strategy string) []*pb.AuditEntry

	// RegisterArtifact upserts an artifact into the catalog (keyed by name+version)
	// with state READY. Prefer RegisterArtifactRecord when setting PENDING/FAILED.
	RegisterArtifact(ref *pb.ArtifactRef) error

	// RegisterArtifactRecord upserts a catalog row including ingest state.
	RegisterArtifactRecord(rec *ArtifactRecord) error

	// GetArtifact looks up a registered artifact by name and version.
	GetArtifact(name, version string) (*pb.ArtifactRef, bool)

	// GetArtifactRecord looks up a catalog row including ingest state.
	GetArtifactRecord(name, version string) (*ArtifactRecord, bool)

	// ListArtifacts returns registered artifacts, optionally filtered by name.
	ListArtifacts(name string) []*pb.ArtifactRef

	// ListArtifactRecords returns catalog rows (ref + state), newest-first per name.
	ListArtifactRecords(name string) []*ArtifactRecord

	// SetArtifactState updates ingest state/reason without changing the ArtifactRef.
	SetArtifactState(name, version, state, reason string) error

	// FinalizeIngest rewrites uri to newURI and marks READY when the row is still
	// PENDING with the expected digest (guards against superseded re-registers).
	FinalizeIngest(name, version, expectedDigest, newURI string) error

	// FailPendingArtifacts marks every PENDING row FAILED with reason
	// (used on control-plane restart; ingest is not resumed).
	FailPendingArtifacts(reason string) (int, error)

	// PreviousArtifact returns the artifact that was replaced by the last Deploy
	// for the given machine/strategy (for empty-target Rollback).
	PreviousArtifact(machineID, strategy string) (*pb.ArtifactRef, bool)

	// AcquireLease grants or refreshes a fencing lease for strategy on machineID.
	// Denied when another machine holds an unexpired lease (including margin_cp).
	AcquireLease(machineID, strategy string, ttl time.Duration) (LeaseResult, error)

	// RenewLease extends a lease; only the current holder with matching lease_id.
	RenewLease(machineID, strategy, leaseID string, ttl time.Duration) (LeaseResult, error)

	// GetLease returns the current lease record for strategy, if any.
	GetLease(strategy string) (LeaseInfo, bool)

	// LeaseMarginCP returns the control-plane lease expiry margin.
	LeaseMarginCP() time.Duration

	// LoadAPITokens returns all active (non-revoked) API tokens.
	LoadAPITokens(ctx context.Context) ([]TokenRow, error)

	// InsertAPIToken persists a newly issued token (hash only).
	InsertAPIToken(ctx context.Context, t TokenRow) error

	// RevokeAPIToken soft-deletes a token owned by userID (sets revoked_at).
	// Returns false when no matching active token exists.
	RevokeAPIToken(ctx context.Context, userID, id string) (bool, error)

	// TouchAPITokens batch-updates last_used for the given token ids.
	// Best-effort telemetry; callers may lose unflushed updates on hard kill.
	TouchAPITokens(ctx context.Context, lastUsed map[string]time.Time) error

	// ApplyAssignmentSet upserts a cluster object. The caller supplies a fully
	// populated metadata+spec; the store assigns uid/created_at on create and
	// increments generation only when spec changes. Labels-only updates do
	// not bump generation. Returns the persisted object and whether it changed.
	ApplyAssignmentSet(cluster *pb.AssignmentSet) (out *pb.AssignmentSet, changed bool, err error)

	// GetAssignmentSet looks up a cluster by metadata.name.
	GetAssignmentSet(name string) (*pb.AssignmentSet, bool)

	// ListAssignmentSets returns all clusters, name-sorted.
	ListAssignmentSets() []*pb.AssignmentSet

	// UpdateAssignmentSetStatus writes controller-observed status.
	UpdateAssignmentSetStatus(name string, status *pb.AssignmentSetStatus) error

	// MarkAssignmentSetDeleting sets status.deleting / phase=Deleting.
	MarkAssignmentSetDeleting(name string) (*pb.AssignmentSet, error)

	// DeleteAssignmentSet removes the row (after the controller undeploys).
	DeleteAssignmentSet(name string) error

	// ReservedBy reports the cluster that owns machineID+strategy, if any.
	ReservedBy(machineID, strategy string) (cluster string, ok bool)

	// ReservedSlots returns every strategy name reserved on machineID.
	ReservedSlots(machineID string) []string
}
