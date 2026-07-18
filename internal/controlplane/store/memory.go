package store

import (
	"fmt"
	"sort"
	"sync"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Memory is an in-memory Store. It is safe for concurrent use; the control
// plane serves many agent streams and human API calls concurrently.
type Memory struct {
	mu        sync.RWMutex
	machines  map[string]*MachineRecord
	artifacts map[string]*pb.ArtifactRef // key = name + "\x00" + version
	audit     []*pb.AuditEntry
	hub       *Hub
}

// NewMemory returns an empty in-memory store that notifies hub on changes.
// hub may be nil (no fan-out).
func NewMemory(hub *Hub) *Memory {
	return &Memory{
		machines:  map[string]*MachineRecord{},
		artifacts: map[string]*pb.ArtifactRef{},
		hub:       hub,
	}
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
	if _, err := artifacturi.ResolveLocal(ref.GetUri()); err != nil {
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
