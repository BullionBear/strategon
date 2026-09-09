package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (m *Memory) notifyClusters() {
	if m.hub != nil {
		m.hub.NotifyClusters()
	}
}

func (m *Memory) ApplyAssignmentSet(cluster *pb.AssignmentSet) (*pb.AssignmentSet, bool, error) {
	if cluster == nil || cluster.GetMetadata().GetName() == "" {
		return nil, false, fmt.Errorf("apply nats cluster: name is required")
	}
	if cluster.GetSpec() == nil {
		return nil, false, fmt.Errorf("apply nats cluster: spec is required")
	}
	name := cluster.GetMetadata().GetName()
	m.mu.Lock()
	cur := m.clusters[name]
	// Ownership is re-checked here, under the write lock, because the API-layer
	// admission check is a separate read. Skipped when the spec is unchanged so
	// a re-apply stays idempotent.
	if cur == nil || !proto.Equal(cur.GetSpec(), cluster.GetSpec()) {
		if err := reservationConflict(clustersByName(m.clusters), cluster); err != nil {
			m.mu.Unlock()
			return nil, false, err
		}
	}
	if cur == nil {
		uid, err := newSetUID()
		if err != nil {
			m.mu.Unlock()
			return nil, false, err
		}
		next := proto.Clone(cluster).(*pb.AssignmentSet)
		next.Metadata.Uid = uid
		next.Metadata.Generation = 1
		next.Metadata.CreatedAt = timestamppb.New(m.now())
		if next.Status == nil {
			next.Status = &pb.AssignmentSetStatus{Phase: "Pending"}
		}
		m.clusters[name] = next
		out := proto.Clone(next).(*pb.AssignmentSet)
		m.mu.Unlock()
		m.notifyClusters()
		return out, true, nil
	}
	specChanged := !proto.Equal(cur.GetSpec(), cluster.GetSpec())
	labelsChanged := !labelsEqual(cur.GetMetadata().GetLabels(), cluster.GetMetadata().GetLabels())
	if !specChanged && !labelsChanged {
		out := proto.Clone(cur).(*pb.AssignmentSet)
		m.mu.Unlock()
		return out, false, nil
	}
	next := proto.Clone(cur).(*pb.AssignmentSet)
	next.Spec = proto.Clone(cluster.GetSpec()).(*pb.AssignmentSetSpec)
	if next.Metadata == nil {
		next.Metadata = &pb.ObjectMeta{Name: name}
	}
	next.Metadata.Labels = cloneLabels(cluster.GetMetadata().GetLabels())
	if specChanged {
		next.Metadata.Generation++
	}
	m.clusters[name] = next
	out := proto.Clone(next).(*pb.AssignmentSet)
	m.mu.Unlock()
	m.notifyClusters()
	return out, true, nil
}

func (m *Memory) GetAssignmentSet(name string) (*pb.AssignmentSet, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clusters[name]
	if !ok {
		return nil, false
	}
	return proto.Clone(c).(*pb.AssignmentSet), true
}

func (m *Memory) ListAssignmentSets() []*pb.AssignmentSet {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clusters))
	for n := range m.clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*pb.AssignmentSet, 0, len(names))
	for _, n := range names {
		out = append(out, proto.Clone(m.clusters[n]).(*pb.AssignmentSet))
	}
	return out
}

func (m *Memory) UpdateAssignmentSetStatus(name string, status *pb.AssignmentSetStatus) error {
	m.mu.Lock()
	cur, ok := m.clusters[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("update nats cluster status: %q not found", name)
	}
	cur.Status = preserveDeleting(cur.Status, status)
	m.mu.Unlock()
	m.notifyClusters()
	return nil
}

func (m *Memory) MarkAssignmentSetDeleting(name string) (*pb.AssignmentSet, error) {
	m.mu.Lock()
	cur, ok := m.clusters[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("delete nats cluster: %q not found", name)
	}
	if cur.Status == nil {
		cur.Status = &pb.AssignmentSetStatus{}
	}
	cur.Status.Deleting = true
	cur.Status.Phase = "Deleting"
	out := proto.Clone(cur).(*pb.AssignmentSet)
	m.mu.Unlock()
	m.notifyClusters()
	return out, nil
}

func (m *Memory) DeleteAssignmentSet(name string) error {
	m.mu.Lock()
	if _, ok := m.clusters[name]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("delete nats cluster: %q not found", name)
	}
	delete(m.clusters, name)
	m.mu.Unlock()
	m.notifyClusters()
	return nil
}

func (m *Memory) ReservedBy(machineID, strategy string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return reservedByLocked(m.clusters, machineID, strategy)
}

func (m *Memory) ReservedSlots(machineID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return reservedSlotsLocked(m.clusters, machineID)
}

// ReservationConflictError reports that a machine+strategy is already owned by
// a different AssignmentSet. Callers map it to FailedPrecondition.
type ReservationConflictError struct {
	MachineID string
	Strategy  string
	Owner     string
}

func (e *ReservationConflictError) Error() string {
	return fmt.Sprintf("machine %q strategy %q is owned by AssignmentSet %q",
		e.MachineID, e.Strategy, e.Owner)
}

// reservationConflict reports whether any slot next claims is already claimed
// by a different, non-deleting cluster in existing.
//
// This is the authoritative ownership guard and must run under the same lock
// (memory) or transaction (Postgres) as the write: an admission check in the
// API layer is a separate read, so two concurrent applies can both pass it and
// both land, leaving two controllers rewriting one machine's assignment on
// every tick. existing is expected in a stable order so the reported owner is
// deterministic when several clusters conflict.
func reservationConflict(existing []*pb.AssignmentSet, next *pb.AssignmentSet) error {
	name := next.GetMetadata().GetName()
	effective := withPersistedAssignmentKey(existing, next)
	nextClaims := claimedSlots(effective)
	for _, c := range existing {
		if c.GetMetadata().GetName() == name || c.GetStatus().GetDeleting() {
			continue
		}
		for s := range claimedSlots(c) {
			if _, ok := nextClaims[s]; !ok {
				continue
			}
			return &ReservationConflictError{
				MachineID: s.machine,
				Strategy:  s.strategy,
				Owner:     c.GetMetadata().GetName(),
			}
		}
	}
	return nil
}

// withPersistedAssignmentKey copies the stored assignment_key onto next so an
// Apply that only sends spec does not look like a brand-new legacy set.
func withPersistedAssignmentKey(existing []*pb.AssignmentSet, next *pb.AssignmentSet) *pb.AssignmentSet {
	effective := proto.Clone(next).(*pb.AssignmentSet)
	if effective.GetStatus() == nil {
		effective.Status = &pb.AssignmentSetStatus{}
	}
	if effective.GetStatus().GetAssignmentKey() != "" {
		return effective
	}
	name := next.GetMetadata().GetName()
	for _, c := range existing {
		if c.GetMetadata().GetName() != name {
			continue
		}
		if k := c.GetStatus().GetAssignmentKey(); k != "" {
			effective.Status.AssignmentKey = k
		}
		break
	}
	return effective
}

type assignmentSlot struct {
	machine, strategy string
}

const AssignmentKeyMember = "member"

// claimedSlots is every (machine, strategy) this set currently owns.
// Member names are always claimed. While assignment_key is empty the catalog
// family name is also claimed on each member machine (unless a member is
// already named that).
func claimedSlots(c *pb.AssignmentSet) map[assignmentSlot]struct{} {
	out := map[assignmentSlot]struct{}{}
	add := func(machine, strategy string) {
		if machine == "" || strategy == "" {
			return
		}
		out[assignmentSlot{machine: machine, strategy: strategy}] = struct{}{}
	}
	for _, srv := range c.GetSpec().GetMembers() {
		add(srv.GetMachine(), srv.GetName())
	}
	if c.GetStatus().GetAssignmentKey() == "" {
		cat := SetStrategy(c)
		for _, srv := range c.GetSpec().GetMembers() {
			if srv.GetName() != cat {
				add(srv.GetMachine(), cat)
			}
		}
	}
	return out
}

// clustersByName returns the cluster map as a name-sorted slice.
func clustersByName(clusters map[string]*pb.AssignmentSet) []*pb.AssignmentSet {
	out := make([]*pb.AssignmentSet, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetMetadata().GetName() < out[j].GetMetadata().GetName()
	})
	return out
}

func reservedByLocked(clusters map[string]*pb.AssignmentSet, machineID, strategy string) (string, bool) {
	for _, name := range clusterNames(clusters) {
		c := clusters[name]
		if c.GetStatus().GetDeleting() {
			continue
		}
		if _, ok := claimedSlots(c)[assignmentSlot{machine: machineID, strategy: strategy}]; ok {
			return c.GetMetadata().GetName(), true
		}
	}
	return "", false
}

func reservedSlotsLocked(clusters map[string]*pb.AssignmentSet, machineID string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, name := range clusterNames(clusters) {
		c := clusters[name]
		if c.GetStatus().GetDeleting() {
			continue
		}
		for s := range claimedSlots(c) {
			if s.machine != machineID {
				continue
			}
			if _, ok := seen[s.strategy]; ok {
				continue
			}
			seen[s.strategy] = struct{}{}
			out = append(out, s.strategy)
		}
	}
	sort.Strings(out)
	return out
}

func clusterNames(clusters map[string]*pb.AssignmentSet) []string {
	out := make([]string, 0, len(clusters))
	for n := range clusters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// preserveDeleting keeps a concurrent MarkAssignmentSetDeleting from being
// overwritten by a controller status write that omitted the flag.
func preserveDeleting(cur, incoming *pb.AssignmentSetStatus) *pb.AssignmentSetStatus {
	var next *pb.AssignmentSetStatus
	if incoming != nil {
		next = proto.Clone(incoming).(*pb.AssignmentSetStatus)
	} else {
		next = &pb.AssignmentSetStatus{}
	}
	if cur != nil && cur.GetDeleting() {
		next.Deleting = true
		if next.Phase != "Deleting" {
			next.Phase = "Deleting"
		}
	}
	if cur != nil && next.GetAssignmentKey() == "" && cur.GetAssignmentKey() != "" {
		next.AssignmentKey = cur.GetAssignmentKey()
	}
	return next
}

// SetStrategy returns the catalog / artifact family name (default nats).
func SetStrategy(c *pb.AssignmentSet) string {
	if s := c.GetSpec().GetStrategy(); s != "" {
		return s
	}
	return "nats"
}

func newSetUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func cloneLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
