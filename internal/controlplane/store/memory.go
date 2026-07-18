package store

import (
	"fmt"
	"sort"
	"sync"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Memory is an in-memory Store. It is safe for concurrent use; the control
// plane serves many agent streams and human API calls concurrently.
type Memory struct {
	mu       sync.RWMutex
	machines map[string]*MachineRecord
	audit    []*pb.AuditEntry
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{machines: map[string]*MachineRecord{}}
}

func (m *Memory) UpsertMachine(reg *pb.Register) (*MachineRecord, error) {
	if reg.GetMachineId() == "" {
		return nil, fmt.Errorf("upsert: empty machine_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.machines[reg.GetMachineId()]
	if rec == nil {
		rec = &MachineRecord{
			MachineID:   reg.GetMachineId(),
			Assignments: map[string]*pb.StrategyAssignmentSpec{},
			Status:      map[string]*pb.StrategyAssignmentStatus{},
		}
		m.machines[reg.GetMachineId()] = rec
	}
	rec.Register = proto.Clone(reg).(*pb.Register)
	rec.AgentVersion = reg.GetAgentVersion()
	rec.Reachable = true
	return snapshotMachine(rec), nil
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
	defer m.mu.Unlock()
	rec, ok := m.machines[machineID]
	if !ok {
		return 0, fmt.Errorf("set assignment: unknown machine %s", machineID)
	}
	if spec == nil {
		delete(rec.Assignments, strategy)
	} else {
		rec.Assignments[strategy] = proto.Clone(spec).(*pb.StrategyAssignmentSpec)
	}
	rec.Generation++ // monotonic bump on every spec change (PROTOCOL §1)
	return rec.Generation, nil
}

func (m *Memory) ApplyStatus(machineID string, report *pb.StatusReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.machines[machineID]
	if !ok {
		return fmt.Errorf("apply status: unknown machine %s", machineID)
	}
	for _, a := range report.GetAssignments() {
		rec.Status[a.GetStrategy()] = proto.Clone(a).(*pb.StrategyAssignmentStatus)
	}
	if report.GetObservedGeneration() > rec.ObservedGen {
		rec.ObservedGen = report.GetObservedGeneration()
	}
	return nil
}

func (m *Memory) ApplyHeartbeat(machineID string, hb *pb.Heartbeat, atUnix int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.machines[machineID]
	if !ok {
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
	return nil
}

func (m *Memory) SetReachable(machineID string, reachable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.machines[machineID]
	if !ok {
		return fmt.Errorf("set reachable: unknown machine %s", machineID)
	}
	rec.Reachable = reachable
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
		MachineID:     rec.MachineID,
		Reachable:     rec.Reachable,
		AgentVersion:  rec.AgentVersion,
		LastHeartbeat: rec.LastHeartbeat,
		Generation:    rec.Generation,
		ObservedGen:   rec.ObservedGen,
		Assignments:   map[string]*pb.StrategyAssignmentSpec{},
		Status:        map[string]*pb.StrategyAssignmentStatus{},
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
	return cp
}
