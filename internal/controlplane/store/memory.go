package store

import (
	"fmt"
	"sort"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Memory is an in-memory Store. It is safe for concurrent use; the control
// plane serves many agent streams and human API calls concurrently.
type Memory struct {
	mu          sync.RWMutex
	machines    map[string]*MachineRecord
	artifacts   map[string]*pb.ArtifactRef // key = name + "\x00" + version
	audit       []*pb.AuditEntry
	leases      map[string]*LeaseInfo // strategy -> lease
	leaseMargin time.Duration
	now         func() time.Time // injectable for tests
	hub         *Hub
}

// NewMemory returns an empty in-memory store that notifies hub on changes.
// hub may be nil (no fan-out).
func NewMemory(hub *Hub) *Memory {
	return &Memory{
		machines:    map[string]*MachineRecord{},
		artifacts:   map[string]*pb.ArtifactRef{},
		leases:      map[string]*LeaseInfo{},
		leaseMargin: DefaultLeaseMarginCP,
		now:         time.Now,
		hub:         hub,
	}
}

// SetLeaseMarginCP sets the control-plane lease expiry margin (SAFETY §2).
func (m *Memory) SetLeaseMarginCP(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d < 0 {
		d = 0
	}
	m.leaseMargin = d
}

// SetClock injects a clock for lease expiry tests.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	m.now = now
}

func (m *Memory) notify(machineID string) {
	if m.hub != nil && machineID != "" {
		m.hub.Notify(machineID)
	}
}

func artifactKey(name, version string) string { return name + "\x00" + version }

func (m *Memory) UpsertMachine(reg *pb.Register) (*MachineRecord, error) {
	if reg.GetMachineId() == "" {
		return nil, fmt.Errorf("upsert: empty machine_id")
	}
	m.mu.Lock()
	rec := m.machines[reg.GetMachineId()]
	if rec == nil {
		rec = &MachineRecord{
			MachineID:         reg.GetMachineId(),
			Assignments:       map[string]*pb.StrategyAssignmentSpec{},
			Status:            map[string]*pb.StrategyAssignmentStatus{},
			PreviousArtifacts: map[string]*pb.ArtifactRef{},
		}
		m.machines[reg.GetMachineId()] = rec
	}
	rec.Register = proto.Clone(reg).(*pb.Register)
	rec.AgentVersion = reg.GetAgentVersion()
	rec.AgentBuildVersion = reg.GetAgentBuildVersion()
	rec.Reachable = true
	snap := snapshotMachine(rec)
	m.mu.Unlock()
	m.notify(reg.GetMachineId())
	return snap, nil
}

func (m *Memory) GetMachine(machineID string) (*MachineRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.machines[machineID]
	if !ok {
		return nil, false
	}
	return snapshotMachine(rec), true
}

func (m *Memory) ListMachines() []*MachineRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.machines))
	for id := range m.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*MachineRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, snapshotMachine(m.machines[id]))
	}
	return out
}

func (m *Memory) DesiredState(machineID string) (*pb.DesiredState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.machines[machineID]
	if !ok {
		return nil, false
	}
	return buildDesiredState(rec), true
}

func (m *Memory) SetAssignment(machineID, strategy string, spec *pb.StrategyAssignmentSpec) (int64, error) {
	m.mu.Lock()
	rec, ok := m.machines[machineID]
	if !ok {
		m.mu.Unlock()
		return 0, fmt.Errorf("set assignment: unknown machine %s", machineID)
	}
	if spec == nil {
		delete(rec.Assignments, strategy)
		delete(rec.PreviousArtifacts, strategy)
		delete(rec.Status, strategy)
	} else {
		if old := rec.Assignments[strategy]; old != nil &&
			old.GetArtifact().GetDigest() != "" &&
			old.GetArtifact().GetDigest() != spec.GetArtifact().GetDigest() {
			rec.PreviousArtifacts[strategy] = proto.Clone(old.GetArtifact()).(*pb.ArtifactRef)
		}
		rec.Assignments[strategy] = proto.Clone(spec).(*pb.StrategyAssignmentSpec)
	}
	rec.Generation++ // monotonic bump on every spec change (PROTOCOL §1)
	gen := rec.Generation
	m.mu.Unlock()
	m.notify(machineID)
	return gen, nil
}

func (m *Memory) ApplyStatus(machineID string, report *pb.StatusReport) error {
	m.mu.Lock()
	rec, ok := m.machines[machineID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("apply status: unknown machine %s", machineID)
	}
	for _, a := range report.GetAssignments() {
		rec.Status[a.GetStrategy()] = proto.Clone(a).(*pb.StrategyAssignmentStatus)
	}
	if report.GetObservedGeneration() > rec.ObservedGen {
		rec.ObservedGen = report.GetObservedGeneration()
	}
	m.mu.Unlock()
	m.notify(machineID)
	return nil
}

func (m *Memory) ApplyHeartbeat(machineID string, hb *pb.Heartbeat, atUnix int64) error {
	m.mu.Lock()
	rec, ok := m.machines[machineID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("apply heartbeat: unknown machine %s", machineID)
	}
	if hb.GetResources() != nil {
		rec.LastResources = proto.Clone(hb.GetResources()).(*pb.MachineResources)
	}
	rec.LastHeartbeat = atUnix
	rec.AgentVersion = hb.GetAgentVersion()
	rec.AgentBuildVersion = hb.GetAgentBuildVersion()
	rec.Reachable = true
	if hb.GetObservedGeneration() > rec.ObservedGen {
		rec.ObservedGen = hb.GetObservedGeneration()
	}
	m.mu.Unlock()
	m.notify(machineID)
	return nil
}

func (m *Memory) SetReachable(machineID string, reachable bool) error {
	m.mu.Lock()
	rec, ok := m.machines[machineID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("set reachable: unknown machine %s", machineID)
	}
	rec.Reachable = reachable
	m.mu.Unlock()
	m.notify(machineID)
	return nil
}

func (m *Memory) AppendAudit(entry *pb.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.GetTimestamp() == nil {
		entry.Timestamp = timestamppb.Now()
	}
	m.audit = append(m.audit, proto.Clone(entry).(*pb.AuditEntry))
	return nil
}

func (m *Memory) ListAudit(machineID, strategy string) []*pb.AuditEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.AuditEntry, 0, len(m.audit))
	for i := len(m.audit) - 1; i >= 0; i-- { // newest first
		e := m.audit[i]
		if machineID != "" && e.GetMachineId() != machineID {
			continue
		}
		if strategy != "" && e.GetStrategy() != strategy {
			continue
		}
		out = append(out, proto.Clone(e).(*pb.AuditEntry))
	}
	return out
}

func (m *Memory) RegisterArtifact(ref *pb.ArtifactRef) error {
	if ref.GetName() == "" || ref.GetVersion() == "" || ref.GetDigest() == "" {
		return fmt.Errorf("register artifact: name, version, and digest are required")
	}
	if ref.GetUri() == "" {
		return fmt.Errorf("register artifact: uri is required")
	}
	if err := artifacturi.Validate(ref.GetUri()); err != nil {
		return fmt.Errorf("register artifact: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.artifacts[artifactKey(ref.GetName(), ref.GetVersion())] = proto.Clone(ref).(*pb.ArtifactRef)
	return nil
}

func (m *Memory) GetArtifact(name, version string) (*pb.ArtifactRef, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ref, ok := m.artifacts[artifactKey(name, version)]
	if !ok {
		return nil, false
	}
	return proto.Clone(ref).(*pb.ArtifactRef), true
}

func (m *Memory) ListArtifacts(name string) []*pb.ArtifactRef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.ArtifactRef, 0, len(m.artifacts))
	for _, ref := range m.artifacts {
		if name != "" && ref.GetName() != name {
			continue
		}
		out = append(out, proto.Clone(ref).(*pb.ArtifactRef))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetName() != out[j].GetName() {
			return out[i].GetName() < out[j].GetName()
		}
		return out[i].GetVersion() < out[j].GetVersion()
	})
	return out
}

func (m *Memory) PreviousArtifact(machineID, strategy string) (*pb.ArtifactRef, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.machines[machineID]
	if !ok {
		return nil, false
	}
	ref, ok := rec.PreviousArtifacts[strategy]
	if !ok || ref == nil {
		return nil, false
	}
	return proto.Clone(ref).(*pb.ArtifactRef), true
}

func (m *Memory) LeaseMarginCP() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.leaseMargin
}

func (m *Memory) GetLease(strategy string) (LeaseInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.leases[strategy]
	if !ok || info == nil {
		return LeaseInfo{}, false
	}
	return *info, true
}

func (m *Memory) AcquireLease(machineID, strategy string, ttl time.Duration) (LeaseResult, error) {
	if machineID == "" || strategy == "" {
		return LeaseResult{}, fmt.Errorf("acquire: machine_id and strategy are required")
	}
	if ttl <= 0 {
		return LeaseResult{}, fmt.Errorf("acquire: ttl must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur := m.leases[strategy]
	if free, deny := leaseFreeFor(cur, machineID, now, m.leaseMargin); !free {
		return LeaseResult{DenyReason: deny}, nil
	}
	id, err := newLeaseID()
	if err != nil {
		return LeaseResult{}, err
	}
	exp := now.Add(ttl)
	m.leases[strategy] = &LeaseInfo{
		Strategy:  strategy,
		MachineID: machineID,
		LeaseID:   id,
		ExpiresAt: exp,
		TTL:       ttl,
	}
	m.notify(machineID)
	return LeaseResult{Granted: true, LeaseID: id, ExpiresAt: exp}, nil
}

func (m *Memory) RenewLease(machineID, strategy, leaseID string, ttl time.Duration) (LeaseResult, error) {
	if machineID == "" || strategy == "" || leaseID == "" {
		return LeaseResult{}, fmt.Errorf("renew: machine_id, strategy, and lease_id are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur := m.leases[strategy]
	if cur == nil {
		return LeaseResult{DenyReason: "no lease"}, nil
	}
	if cur.MachineID != machineID || cur.LeaseID != leaseID {
		return LeaseResult{DenyReason: denyHeld(cur.MachineID, cur.ExpiresAt.Add(m.leaseMargin))}, nil
	}
	if !now.Before(cur.ExpiresAt) {
		return LeaseResult{DenyReason: "lease expired"}, nil
	}
	if ttl <= 0 {
		ttl = cur.TTL
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	exp := now.Add(ttl)
	cur.ExpiresAt = exp
	cur.TTL = ttl
	m.notify(machineID)
	return LeaseResult{Granted: true, LeaseID: leaseID, ExpiresAt: exp}, nil
}

// buildDesiredState renders the full snapshot for a machine (caller holds lock).
func buildDesiredState(rec *MachineRecord) *pb.DesiredState {
	names := make([]string, 0, len(rec.Assignments))
	for n := range rec.Assignments {
		names = append(names, n)
	}
	sort.Strings(names)
	assignments := make([]*pb.StrategyAssignmentSpec, 0, len(names))
	for _, n := range names {
		assignments = append(assignments, proto.Clone(rec.Assignments[n]).(*pb.StrategyAssignmentSpec))
	}
	return &pb.DesiredState{
		Generation:  rec.Generation,
		Assignments: assignments,
		IssuedAt:    timestamppb.Now(),
	}
}

// snapshotMachine returns a deep copy (caller holds lock).
func snapshotMachine(rec *MachineRecord) *MachineRecord {
	cp := &MachineRecord{
		MachineID:         rec.MachineID,
		Reachable:         rec.Reachable,
		AgentVersion:      rec.AgentVersion,
		AgentBuildVersion: rec.AgentBuildVersion,
		LastHeartbeat:     rec.LastHeartbeat,
		Generation:        rec.Generation,
		ObservedGen:       rec.ObservedGen,
		Assignments:       map[string]*pb.StrategyAssignmentSpec{},
		Status:            map[string]*pb.StrategyAssignmentStatus{},
		PreviousArtifacts: map[string]*pb.ArtifactRef{},
	}
	if rec.Register != nil {
		cp.Register = proto.Clone(rec.Register).(*pb.Register)
	}
	if rec.LastResources != nil {
		cp.LastResources = proto.Clone(rec.LastResources).(*pb.MachineResources)
	}
	for k, v := range rec.Assignments {
		cp.Assignments[k] = proto.Clone(v).(*pb.StrategyAssignmentSpec)
	}
	for k, v := range rec.Status {
		cp.Status[k] = proto.Clone(v).(*pb.StrategyAssignmentStatus)
	}
	for k, v := range rec.PreviousArtifacts {
		cp.PreviousArtifacts[k] = proto.Clone(v).(*pb.ArtifactRef)
	}
	return cp
}
