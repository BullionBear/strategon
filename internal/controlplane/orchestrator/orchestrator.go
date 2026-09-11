// Package orchestrator reconciles AssignmentSet objects into rolling
// per-member assignments. It never talks to agents except through
// assign.Service (the same write path as human verbs).
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/assign"
	"github.com/bullionbear/strategon/internal/controlplane/assignmentset"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/controlplane/view"
	"github.com/bullionbear/strategon/internal/volume"
	"google.golang.org/protobuf/proto"
)

const defaultTick = 2 * time.Second

// Controller is a level-triggered AssignmentSet reconciler.
type Controller struct {
	Store  store.Store
	Assign *assign.Service
	Hub    *store.Hub
	Logger *slog.Logger
	Tick   time.Duration
	Now    func() time.Time

	mu        sync.Mutex
	deadlines map[inflightKey]time.Time
}

type inflightKey struct {
	cluster, machine, name string
}

type assignmentSlot struct {
	machine, strategy string
}

type specMember struct {
	idx int
	srv *pb.SetMember
}

// New constructs a controller. Hub may be nil (tests that call ReconcileAll).
func New(st store.Store, asg *assign.Service, hub *store.Hub, logger *slog.Logger) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		Store:     st,
		Assign:    asg,
		Hub:       hub,
		Logger:    logger,
		Tick:      defaultTick,
		Now:       time.Now,
		deadlines: map[inflightKey]time.Time{},
	}
}

// Run watches Hub + a tick until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) {
	c.ReconcileAll(ctx)
	var all <-chan string
	var clusters <-chan struct{}
	var cancelAll, cancelClusters func()
	if c.Hub != nil {
		all, cancelAll = c.Hub.SubscribeAll()
		defer cancelAll()
		clusters, cancelClusters = c.Hub.SubscribeClusters()
		defer cancelClusters()
	}
	tick := c.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-all:
			c.ReconcileAll(ctx)
		case <-clusters:
			c.ReconcileAll(ctx)
		case <-t.C:
			c.ReconcileAll(ctx)
		}
	}
}

// ReconcileAll reconciles every stored cluster.
func (c *Controller) ReconcileAll(ctx context.Context) {
	for _, cl := range c.Store.ListAssignmentSets() {
		if err := c.Reconcile(ctx, cl); err != nil {
			c.Logger.Warn("assignment set reconcile", "cluster", cl.GetMetadata().GetName(), "err", err)
		}
	}
}

// Reconcile brings one cluster toward its spec.
func (c *Controller) Reconcile(ctx context.Context, cl *pb.AssignmentSet) error {
	if cl.GetStatus().GetDeleting() {
		return c.reconcileDelete(ctx, cl)
	}
	art, cfg, err := resolveClusterArtifacts(c.Store, cl)
	if err != nil {
		return c.setStatus(cl, &pb.AssignmentSetStatus{
			Phase:              "Failed",
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "Artifact",
			Message:            err.Error(),
			Members:            cl.GetStatus().GetMembers(),
			AssignmentKey:      cl.GetStatus().GetAssignmentKey(),
		})
	}

	ordered := orderedSpecMembers(cl)

	type member struct {
		srv      *pb.SetMember
		computed *pb.StrategyAssignmentSpec
		view     *pb.StrategyView
		live     *pb.StrategyAssignmentSpec
		legacy   *pb.StrategyAssignmentSpec
		rec      *store.MachineRecord
	}
	maxUnavail := cl.GetSpec().GetUpdate().GetMaxUnavailable()
	if maxUnavail < 1 {
		maxUnavail = 1
	}
	dropped := c.droppedSlots(cl)
	undeployed, err := c.undeploySlots(ctx, cl, dropped, int(maxUnavail))
	if err != nil {
		return c.setAssignFailed(cl, cl.GetStatus().GetMembers(), err)
	}

	members := make([]member, 0, len(ordered))
	var candidates []member
	var inflight []member
	serverStatus := make([]*pb.MemberStatus, 0, len(ordered))

	for _, item := range ordered {
		srv := item.srv
		computed, err := computeAssignment(cl, item.idx, art, cfg)
		if err != nil {
			return c.setAssignFailed(cl, cl.GetStatus().GetMembers(), err)
		}
		rec, ok := c.Store.GetMachine(srv.GetMachine())
		if !ok {
			rec = &store.MachineRecord{MachineID: srv.GetMachine(), Assignments: map[string]*pb.StrategyAssignmentSpec{}, Status: map[string]*pb.StrategyAssignmentStatus{}}
		}
		if err := volume.EnsureInventory(srv.GetMachine(), rec.Volumes, computed.GetVolumeMounts()); err != nil {
			return c.setAssignFailed(cl, cl.GetStatus().GetMembers(), err)
		}
		slotName := srv.GetName()
		live := rec.Assignments[slotName]
		sv := view.BuildStrategyView(rec, slotName, c.Store, nil, nil)
		m := member{srv: srv, computed: computed, view: sv, live: live, rec: rec}
		if fam, ok := familyLeftover(cl, srv.GetMachine()); ok {
			m.legacy = rec.Assignments[fam]
		}
		members = append(members, m)
		ready := view.IsConverged(sv) && view.ReadyConditionTrue(sv)
		match := live != nil && proto.Equal(live, computed)
		serverStatus = append(serverStatus, &pb.MemberStatus{
			Machine:   srv.GetMachine(),
			Name:      srv.GetName(),
			Ready:     ready,
			Phase:     sv.GetPhase().String(),
			Converged: sv.GetConverged(),
		})
		if !match {
			candidates = append(candidates, m)
			continue
		}
		if !ready {
			inflight = append(inflight, m)
			continue
		}
		c.clearDeadline(cl.GetMetadata().GetName(), srv.GetMachine(), srv.GetName())
	}

	waitSec := waitReadySeconds(cl)

	now := c.Now()
	stopReason := ""
	stopPhase := ""
	for _, m := range inflight {
		phase := m.view.GetPhase()
		if phase == pb.DeployPhase_DEPLOY_PHASE_FAILED || phase == pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK {
			stopPhase = "Failed"
			stopReason = fmt.Sprintf("member %s/%s phase %s", m.srv.GetMachine(), m.srv.GetName(), phase)
			break
		}
		key := inflightKey{cl.GetMetadata().GetName(), m.srv.GetMachine(), m.srv.GetName()}
		c.mu.Lock()
		dl, ok := c.deadlines[key]
		if !ok {
			dl = now.Add(time.Duration(waitSec) * time.Second)
			c.deadlines[key] = dl
		}
		c.mu.Unlock()
		if now.After(dl) && !view.ReadyConditionTrue(m.view) {
			stopPhase = "Degraded"
			stopReason = fmt.Sprintf("member %s/%s waitReadySeconds elapsed", m.srv.GetMachine(), m.srv.GetName())
			break
		}
	}
	if stopPhase != "" {
		return c.setStatus(cl, &pb.AssignmentSetStatus{
			Phase:              stopPhase,
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "RollStopped",
			Message:            stopReason,
			Members:            c.withRetainedDrops(cl, serverStatus),
			AssignmentKey:      cl.GetStatus().GetAssignmentKey(),
		})
	}

	wrote := 0
	budget := int(maxUnavail) - len(inflight) - undeployed
	if budget < 0 {
		budget = 0
	}
	for _, m := range candidates {
		if wrote >= budget {
			break
		}
		if m.legacy != nil {
			if err := c.undeploySlot(ctx, cl, assignmentSlot{m.srv.GetMachine(), store.SetStrategy(cl)}); err != nil {
				return c.setAssignFailed(cl, serverStatus, err)
			}
		}
		detail := fmt.Sprintf("cluster=%s generation=%d", cl.GetMetadata().GetName(), cl.GetMetadata().GetGeneration())
		if _, _, err := c.Assign.Apply(ctx, assign.Request{
			MachineID:             m.srv.GetMachine(),
			Strategy:              m.srv.GetName(),
			Spec:                  m.computed,
			Action:                "AssignmentSet",
			Detail:                detail,
			FromVersion:           m.live.GetArtifact().GetVersion(),
			ToVersion:             m.computed.GetArtifact().GetVersion(),
			EnforceLeaseInterlock: false,
			AllowReserved:         true,
		}); err != nil {
			return c.setAssignFailed(cl, serverStatus, err)
		}
		c.mu.Lock()
		c.deadlines[inflightKey{cl.GetMetadata().GetName(), m.srv.GetMachine(), m.srv.GetName()}] = now.Add(time.Duration(waitSec) * time.Second)
		c.mu.Unlock()
		wrote++
	}

	droppedLeft := len(c.droppedSlots(cl))
	allReady := len(candidates) == 0 && len(inflight) == 0 && len(members) > 0 && droppedLeft == 0
	phase := "Rolling"
	obs := cl.GetStatus().GetObservedGeneration()
	if allReady {
		phase = "Ready"
		obs = cl.GetMetadata().GetGeneration()
		c.pruneClusterDeadlines(cl.GetMetadata().GetName())
	} else if len(members) == 0 {
		phase = "Pending"
	} else if wrote == 0 && undeployed == 0 && len(inflight) == 0 && len(candidates) > 0 {
		phase = "Pending"
	}
	key := cl.GetStatus().GetAssignmentKey()
	if c.transitionComplete(cl) {
		key = store.AssignmentKeyMember
	}
	return c.setStatus(cl, &pb.AssignmentSetStatus{
		Phase:              phase,
		ObservedGeneration: obs,
		Members:            c.withRetainedDrops(cl, serverStatus),
		AssignmentKey:      key,
	})
}

func (c *Controller) reconcileDelete(ctx context.Context, cl *pb.AssignmentSet) error {
	maxUnavail := cl.GetSpec().GetUpdate().GetMaxUnavailable()
	if maxUnavail < 1 {
		maxUnavail = 1
	}
	targets := c.assignedSlots(cl)
	removed, err := c.undeploySlots(ctx, cl, targets, int(maxUnavail))
	if err != nil {
		return c.setAssignFailed(cl, cl.GetStatus().GetMembers(), err)
	}
	if len(targets) > removed {
		return c.setStatus(cl, &pb.AssignmentSetStatus{
			Phase:              "Deleting",
			Deleting:           true,
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "Undeploying",
			Members:            cl.GetStatus().GetMembers(),
			AssignmentKey:      cl.GetStatus().GetAssignmentKey(),
		})
	}
	c.pruneClusterDeadlines(cl.GetMetadata().GetName())
	return c.Store.DeleteAssignmentSet(cl.GetMetadata().GetName())
}

func (c *Controller) setAssignFailed(cl *pb.AssignmentSet, servers []*pb.MemberStatus, err error) error {
	return c.setStatus(cl, &pb.AssignmentSetStatus{
		Phase:              "Failed",
		ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
		Reason:             "AssignFailed",
		Message:            err.Error(),
		Members:            c.withRetainedDrops(cl, servers),
		AssignmentKey:      cl.GetStatus().GetAssignmentKey(),
	})
}

func (c *Controller) undeploySlots(ctx context.Context, cl *pb.AssignmentSet, slots []assignmentSlot, limit int) (int, error) {
	removed := 0
	for _, s := range slots {
		rec, ok := c.Store.GetMachine(s.machine)
		if !ok || rec.Assignments[s.strategy] == nil {
			c.clearDeadline(cl.GetMetadata().GetName(), s.machine, s.strategy)
			continue
		}
		if removed >= limit {
			continue
		}
		if err := c.undeploySlot(ctx, cl, s); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (c *Controller) undeploySlot(ctx context.Context, cl *pb.AssignmentSet, s assignmentSlot) error {
	if _, _, err := c.Assign.Apply(ctx, assign.Request{
		MachineID:     s.machine,
		Strategy:      s.strategy,
		Spec:          nil,
		Action:        "AssignmentSetUndeploy",
		Detail:        cl.GetMetadata().GetName(),
		AllowReserved: true,
	}); err != nil {
		return err
	}
	c.clearDeadline(cl.GetMetadata().GetName(), s.machine, s.strategy)
	return nil
}

func (c *Controller) droppedSlots(cl *pb.AssignmentSet) []assignmentSlot {
	want := specSlots(cl)
	var out []assignmentSlot
	for _, s := range c.assignedSlots(cl) {
		if _, ok := want[s]; ok {
			continue
		}
		if _, ok := familyLeftover(cl, s.machine); ok && s.strategy == store.SetStrategy(cl) {
			// Family leftover on a machine that still has spec members is a
			// paired replace, not a drop.
			continue
		}
		out = append(out, s)
	}
	return out
}

func (c *Controller) assignedSlots(cl *pb.AssignmentSet) []assignmentSlot {
	var out []assignmentSlot
	for _, s := range c.ownedSlots(cl) {
		rec, ok := c.Store.GetMachine(s.machine)
		if !ok || rec.Assignments[s.strategy] == nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (c *Controller) ownedSlots(cl *pb.AssignmentSet) []assignmentSlot {
	seen := map[assignmentSlot]struct{}{}
	var out []assignmentSlot
	add := func(machine, strategy string) {
		if machine == "" || strategy == "" {
			return
		}
		s := assignmentSlot{machine: machine, strategy: strategy}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, m := range cl.GetSpec().GetMembers() {
		add(m.GetMachine(), m.GetName())
	}
	for _, m := range cl.GetStatus().GetMembers() {
		add(m.GetMachine(), m.GetName())
	}
	if !usesMemberKey(cl) {
		cat := store.SetStrategy(cl)
		machines := map[string]struct{}{}
		for _, m := range cl.GetSpec().GetMembers() {
			machines[m.GetMachine()] = struct{}{}
		}
		for _, m := range cl.GetStatus().GetMembers() {
			machines[m.GetMachine()] = struct{}{}
		}
		for machine := range machines {
			namedFamily := false
			for _, m := range cl.GetSpec().GetMembers() {
				if m.GetMachine() == machine && m.GetName() == cat {
					namedFamily = true
					break
				}
			}
			if namedFamily {
				continue
			}
			add(machine, cat)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].machine != out[j].machine {
			return out[i].machine < out[j].machine
		}
		return out[i].strategy < out[j].strategy
	})
	return out
}

func specSlots(cl *pb.AssignmentSet) map[assignmentSlot]struct{} {
	out := map[assignmentSlot]struct{}{}
	for _, s := range cl.GetSpec().GetMembers() {
		out[assignmentSlot{machine: s.GetMachine(), strategy: s.GetName()}] = struct{}{}
	}
	return out
}

// withRetainedDrops appends status rows for assigned slots that are no longer
// in spec. Reconcile used to overwrite status.members with spec-only rows in
// the same tick as a partial undeploy, so extra drops vanished from ownedSlots
// and were never undeployed.
func (c *Controller) withRetainedDrops(cl *pb.AssignmentSet, specStatus []*pb.MemberStatus) []*pb.MemberStatus {
	if cl == nil {
		return specStatus
	}
	want := specSlots(cl)
	seen := map[assignmentSlot]struct{}{}
	out := make([]*pb.MemberStatus, 0, len(specStatus)+4)
	for _, s := range specStatus {
		if s == nil {
			continue
		}
		seen[assignmentSlot{machine: s.GetMachine(), strategy: s.GetName()}] = struct{}{}
		out = append(out, s)
	}
	for _, s := range c.droppedSlots(cl) {
		if _, ok := want[s]; ok {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		rec, ok := c.Store.GetMachine(s.machine)
		if !ok || rec.Assignments[s.strategy] == nil {
			continue
		}
		sv := view.BuildStrategyView(rec, s.strategy, c.Store, nil, nil)
		out = append(out, &pb.MemberStatus{
			Machine:   s.machine,
			Name:      s.strategy,
			Ready:     view.IsConverged(sv) && view.ReadyConditionTrue(sv),
			Phase:     sv.GetPhase().String(),
			Converged: sv.GetConverged(),
		})
		seen[s] = struct{}{}
	}
	return out
}

func (c *Controller) transitionComplete(cl *pb.AssignmentSet) bool {
	if len(cl.GetSpec().GetMembers()) == 0 {
		return false
	}
	for _, m := range cl.GetSpec().GetMembers() {
		rec, ok := c.Store.GetMachine(m.GetMachine())
		if !ok || rec.Assignments[m.GetName()] == nil {
			return false
		}
	}
	// Dropped machines still holding a family (or member) slot must finish
	// draining before the key flips; otherwise ownedSlots stops emitting
	// (machine, catalog) and the process is orphaned.
	if len(c.droppedSlots(cl)) > 0 {
		return false
	}
	if !usesMemberKey(cl) {
		for _, s := range c.assignedSlots(cl) {
			if s.strategy == store.SetStrategy(cl) && !specHasMember(cl, s.machine, s.strategy) {
				return false
			}
		}
	}
	return true
}

func specHasMember(cl *pb.AssignmentSet, machine, name string) bool {
	for _, m := range cl.GetSpec().GetMembers() {
		if m.GetMachine() == machine && m.GetName() == name {
			return true
		}
	}
	return false
}

func (c *Controller) clearDeadline(cluster, machine, name string) {
	c.mu.Lock()
	delete(c.deadlines, inflightKey{cluster, machine, name})
	c.mu.Unlock()
}

func (c *Controller) pruneClusterDeadlines(cluster string) {
	c.mu.Lock()
	for k := range c.deadlines {
		if k.cluster == cluster {
			delete(c.deadlines, k)
		}
	}
	c.mu.Unlock()
}

func (c *Controller) setStatus(cl *pb.AssignmentSet, st *pb.AssignmentSetStatus) error {
	if st.GetAssignmentKey() == "" {
		st.AssignmentKey = cl.GetStatus().GetAssignmentKey()
	}
	if proto.Equal(cl.GetStatus(), st) {
		return nil
	}
	return c.Store.UpdateAssignmentSetStatus(cl.GetMetadata().GetName(), st)
}

func computeAssignment(cl *pb.AssignmentSet, idx int, art, cfg *pb.ArtifactRef) (*pb.StrategyAssignmentSpec, error) {
	rendered, err := assignmentset.Expand(cl, idx)
	if err != nil {
		return nil, err
	}
	tmpl := cl.GetSpec().GetTemplate()
	members := cl.GetSpec().GetMembers()
	if idx < 0 || idx >= len(members) {
		return nil, fmt.Errorf("member index %d out of range", idx)
	}
	spec := &pb.StrategyAssignmentSpec{
		Strategy: members[idx].GetName(),
		Artifact: proto.Clone(art).(*pb.ArtifactRef),
		Stopped:  false,
		Args:     rendered.Args,
		Env:      rendered.Env,
	}
	if p := tmpl.GetDeployPolicy(); p != nil {
		spec.DeployPolicy = proto.Clone(p).(*pb.DeployPolicy)
	} else {
		spec.DeployPolicy = defaultDeployPolicy(cl)
	}
	if spec.GetDeployPolicy().GetHealthWindowSeconds() <= 0 {
		spec.DeployPolicy.HealthWindowSeconds = waitReadySeconds(cl)
	}
	if l := tmpl.GetLimits(); l != nil {
		spec.Limits = proto.Clone(l).(*pb.ResourceLimits)
	}
	if rendered.Endpoint != "" {
		spec.Readiness = &pb.ReadinessProbe{Endpoint: rendered.Endpoint}
	}
	if cfg != nil {
		spec.Config = proto.Clone(cfg).(*pb.ArtifactRef)
	}
	if art.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE {
		spec.Driver = pb.ExecutionDriver_EXECUTION_DRIVER_OCI
	} else {
		spec.Driver = pb.ExecutionDriver_EXECUTION_DRIVER_EXEC
	}
	if len(rendered.VolumeMounts) > 0 {
		spec.VolumeMounts = rendered.VolumeMounts
	}
	return spec, nil
}

// defaultDeployPolicy is used when the template omits one. Auto-rollback is on:
// on a first roll there is no previous version, so the agent reports FAILED
// (an honest terminal signal); on an upgrade the member returns on the previous
// version rather than staying dead. Either way the roll stops.
func defaultDeployPolicy(cl *pb.AssignmentSet) *pb.DeployPolicy {
	return &pb.DeployPolicy{
		Startsecs:           2,
		HealthWindowSeconds: waitReadySeconds(cl),
		MaxCrashesInWindow:  3,
		StopGraceSeconds:    10,
		EnableAutoRollback:  true,
	}
}

func waitReadySeconds(cl *pb.AssignmentSet) int32 {
	if w := cl.GetSpec().GetUpdate().GetWaitReadySeconds(); w > 0 {
		return w
	}
	return 60
}

func resolveClusterArtifacts(st store.Store, cl *pb.AssignmentSet) (art, cfg *pb.ArtifactRef, err error) {
	strategy := store.SetStrategy(cl)
	ver := cl.GetSpec().GetArtifactVersion()
	art, ok := st.GetArtifact(strategy, ver)
	if !ok {
		return nil, nil, fmt.Errorf("artifact %s@%s not registered", strategy, ver)
	}
	if rec, ok := st.GetArtifactRecord(art.GetName(), art.GetVersion()); ok && rec.State != store.ArtifactStateReady {
		return nil, nil, fmt.Errorf("artifact %s@%s is %s", art.GetName(), art.GetVersion(), rec.State)
	}
	if cv := cl.GetSpec().GetConfigVersion(); cv != "" {
		cfg, ok = st.GetArtifact(art.GetName()+"-config", cv)
		if !ok {
			cfg, ok = st.GetArtifact(strategy+"-config", cv)
		}
		if !ok {
			return nil, nil, fmt.Errorf("config %s@%s not registered", strategy+"-config", cv)
		}
	}
	return art, cfg, nil
}

// orderedSpecMembers sorts members by (machine, name) while keeping the
// original spec index so Expand/computeAssignment read the right entry.
func orderedSpecMembers(cl *pb.AssignmentSet) []specMember {
	raw := cl.GetSpec().GetMembers()
	out := make([]specMember, len(raw))
	for i, srv := range raw {
		out[i] = specMember{idx: i, srv: srv}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.srv.GetMachine() != b.srv.GetMachine() {
			return a.srv.GetMachine() < b.srv.GetMachine()
		}
		if a.srv.GetName() != b.srv.GetName() {
			return a.srv.GetName() < b.srv.GetName()
		}
		return a.idx < b.idx
	})
	return out
}

func usesMemberKey(cl *pb.AssignmentSet) bool {
	return cl.GetStatus().GetAssignmentKey() == store.AssignmentKeyMember
}

// familyLeftover reports the catalog slot still owned on machine while the
// set has not flipped assignment_key, and no spec member on that machine is
// already named the catalog.
func familyLeftover(cl *pb.AssignmentSet, machine string) (string, bool) {
	if usesMemberKey(cl) {
		return "", false
	}
	cat := store.SetStrategy(cl)
	owned := false
	for _, m := range cl.GetSpec().GetMembers() {
		if m.GetMachine() != machine {
			continue
		}
		owned = true
		if m.GetName() == cat {
			return "", false
		}
	}
	if !owned {
		return "", false
	}
	return cat, true
}
