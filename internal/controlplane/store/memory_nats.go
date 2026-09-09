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

func (m *Memory) ApplyNatsCluster(cluster *pb.NatsCluster) (*pb.NatsCluster, bool, error) {
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
		uid, err := newClusterUID()
		if err != nil {
			m.mu.Unlock()
			return nil, false, err
		}
		next := proto.Clone(cluster).(*pb.NatsCluster)
		next.Metadata.Uid = uid
		next.Metadata.Generation = 1
		next.Metadata.CreatedAt = timestamppb.New(m.now())
		if next.Status == nil {
			next.Status = &pb.NatsClusterStatus{Phase: "Pending"}
		}
		m.clusters[name] = next
		out := proto.Clone(next).(*pb.NatsCluster)
		m.mu.Unlock()
		m.notifyClusters()
		return out, true, nil
	}
	specChanged := !proto.Equal(cur.GetSpec(), cluster.GetSpec())
	labelsChanged := !labelsEqual(cur.GetMetadata().GetLabels(), cluster.GetMetadata().GetLabels())
	if !specChanged && !labelsChanged {
		out := proto.Clone(cur).(*pb.NatsCluster)
		m.mu.Unlock()
		return out, false, nil
	}
	next := proto.Clone(cur).(*pb.NatsCluster)
	next.Spec = proto.Clone(cluster.GetSpec()).(*pb.NatsClusterSpec)
	if next.Metadata == nil {
		next.Metadata = &pb.ObjectMeta{Name: name}
	}
	next.Metadata.Labels = cloneLabels(cluster.GetMetadata().GetLabels())
	if specChanged {
		next.Metadata.Generation++
	}
	m.clusters[name] = next
	out := proto.Clone(next).(*pb.NatsCluster)
	m.mu.Unlock()
	m.notifyClusters()
	return out, true, nil
}

func (m *Memory) GetNatsCluster(name string) (*pb.NatsCluster, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clusters[name]
	if !ok {
		return nil, false
	}
	return proto.Clone(c).(*pb.NatsCluster), true
}

func (m *Memory) ListNatsClusters() []*pb.NatsCluster {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clusters))
	for n := range m.clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*pb.NatsCluster, 0, len(names))
	for _, n := range names {
		out = append(out, proto.Clone(m.clusters[n]).(*pb.NatsCluster))
	}
	return out
}

func (m *Memory) UpdateNatsClusterStatus(name string, status *pb.NatsClusterStatus) error {
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

func (m *Memory) MarkNatsClusterDeleting(name string) (*pb.NatsCluster, error) {
	m.mu.Lock()
	cur, ok := m.clusters[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("delete nats cluster: %q not found", name)
	}
	if cur.Status == nil {
		cur.Status = &pb.NatsClusterStatus{}
	}
	cur.Status.Deleting = true
	cur.Status.Phase = "Deleting"
	out := proto.Clone(cur).(*pb.NatsCluster)
	m.mu.Unlock()
	m.notifyClusters()
	return out, nil
}

func (m *Memory) DeleteNatsCluster(name string) error {
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

// ReservationConflictError reports that a machine+strategy is already owned by
// a different NatsCluster. Callers map it to FailedPrecondition.
type ReservationConflictError struct {
	MachineID string
	Strategy  string
	Owner     string
}

func (e *ReservationConflictError) Error() string {
	return fmt.Sprintf("machine %q strategy %q is owned by NatsCluster %q",
		e.MachineID, e.Strategy, e.Owner)
}

// reservationConflict reports whether any machine in next's spec is already
// claimed by a different, non-deleting cluster in existing.
//
// This is the authoritative ownership guard and must run under the same lock
// (memory) or transaction (Postgres) as the write: an admission check in the
// API layer is a separate read, so two concurrent applies can both pass it and
// both land, leaving two controllers rewriting one machine's assignment on
// every tick. existing is expected in a stable order so the reported owner is
// deterministic when several clusters conflict.
func reservationConflict(existing []*pb.NatsCluster, next *pb.NatsCluster) error {
	strategy := ClusterStrategy(next)
	name := next.GetMetadata().GetName()
	for _, srv := range next.GetSpec().GetServers() {
		for _, c := range existing {
			if c.GetMetadata().GetName() == name || c.GetStatus().GetDeleting() {
				continue
			}
			if ClusterStrategy(c) != strategy {
				continue
			}
			for _, es := range c.GetSpec().GetServers() {
				if es.GetMachine() != srv.GetMachine() {
					continue
				}
				return &ReservationConflictError{
					MachineID: srv.GetMachine(),
					Strategy:  strategy,
					Owner:     c.GetMetadata().GetName(),
				}
			}
		}
	}
	return nil
}

// clustersByName returns the cluster map as a name-sorted slice.
func clustersByName(clusters map[string]*pb.NatsCluster) []*pb.NatsCluster {
	out := make([]*pb.NatsCluster, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetMetadata().GetName() < out[j].GetMetadata().GetName()
	})
	return out
}

func reservedByLocked(clusters map[string]*pb.NatsCluster, machineID, strategy string) (string, bool) {
	for _, c := range clusters {
		if c.GetStatus().GetDeleting() {
			continue
		}
		strat := ClusterStrategy(c)
		if strat != strategy {
			continue
		}
		for _, srv := range c.GetSpec().GetServers() {
			if srv.GetMachine() == machineID {
				return c.GetMetadata().GetName(), true
			}
		}
	}
	return "", false
}

// preserveDeleting keeps a concurrent MarkNatsClusterDeleting from being
// overwritten by a controller status write that omitted the flag.
func preserveDeleting(cur, incoming *pb.NatsClusterStatus) *pb.NatsClusterStatus {
	var next *pb.NatsClusterStatus
	if incoming != nil {
		next = proto.Clone(incoming).(*pb.NatsClusterStatus)
	} else {
		next = &pb.NatsClusterStatus{}
	}
	if cur != nil && cur.GetDeleting() {
		next.Deleting = true
		if next.Phase != "Deleting" {
			next.Phase = "Deleting"
		}
	}
	return next
}

// ClusterStrategy returns the owned strategy name (default nats).
func ClusterStrategy(c *pb.NatsCluster) string {
	if s := c.GetSpec().GetStrategy(); s != "" {
		return s
	}
	return "nats"
}

func newClusterUID() (string, error) {
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
