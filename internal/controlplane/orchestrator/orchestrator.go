// Package orchestrator reconciles NatsCluster objects into rolling
// per-machine assignments. It never talks to agents except through
// assign.Service (the same write path as human verbs).
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/assign"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/controlplane/view"
	"google.golang.org/protobuf/proto"
)

const defaultTick = 2 * time.Second

// Controller is a level-triggered NatsCluster reconciler.
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
	cluster, machine string
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
	for _, cl := range c.Store.ListNatsClusters() {
		if err := c.Reconcile(ctx, cl); err != nil {
			c.Logger.Warn("nats cluster reconcile", "cluster", cl.GetMetadata().GetName(), "err", err)
		}
	}
}

// Reconcile brings one cluster toward its spec.
func (c *Controller) Reconcile(ctx context.Context, cl *pb.NatsCluster) error {
	if cl.GetStatus().GetDeleting() {
		return c.reconcileDelete(ctx, cl)
	}
	strategy := store.ClusterStrategy(cl)
	art, cfg, err := resolveClusterArtifacts(c.Store, cl)
	if err != nil {
		return c.setStatus(cl, &pb.NatsClusterStatus{
			Phase:              "Failed",
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "Artifact",
			Message:            err.Error(),
			Servers:            cl.GetStatus().GetServers(),
		})
	}

	servers := append([]*pb.NatsServer(nil), cl.GetSpec().GetServers()...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].GetMachine() < servers[j].GetMachine() })

	type member struct {
		srv      *pb.NatsServer
		computed *pb.StrategyAssignmentSpec
		view     *pb.StrategyView
		live     *pb.StrategyAssignmentSpec
		rec      *store.MachineRecord
	}
	maxUnavail := cl.GetSpec().GetUpdate().GetMaxUnavailable()
	if maxUnavail < 1 {
		maxUnavail = 1
	}
	dropped := c.droppedMachines(cl)
	undeployed, err := c.undeployMachines(ctx, cl, dropped, int(maxUnavail))
	if err != nil {
		return c.setAssignFailed(cl, cl.GetStatus().GetServers(), err)
	}

	members := make([]member, 0, len(servers))
	var candidates []member
	var inflight []member
	serverStatus := make([]*pb.NatsServerStatus, 0, len(servers))

	for _, srv := range servers {
		computed := computeAssignment(cl, srv, servers, art, cfg)
		rec, ok := c.Store.GetMachine(srv.GetMachine())
		if !ok {
			rec = &store.MachineRecord{MachineID: srv.GetMachine(), Assignments: map[string]*pb.StrategyAssignmentSpec{}, Status: map[string]*pb.StrategyAssignmentStatus{}}
		}
		live := rec.Assignments[strategy]
		sv := view.BuildStrategyView(rec, strategy, c.Store, nil, nil)
		m := member{srv: srv, computed: computed, view: sv, live: live, rec: rec}
		members = append(members, m)
		ready := view.IsConverged(sv) && view.ReadyConditionTrue(sv)
		match := live != nil && proto.Equal(live, computed)
		serverStatus = append(serverStatus, &pb.NatsServerStatus{
			Machine:    srv.GetMachine(),
			ServerName: srv.GetServerName(),
			Ready:      ready,
			Phase:      sv.GetPhase().String(),
			Converged:  sv.GetConverged(),
		})
		if !match {
			candidates = append(candidates, m)
			continue
		}
		if !ready {
			inflight = append(inflight, m)
			continue
		}
		c.clearDeadline(cl.GetMetadata().GetName(), srv.GetMachine())
	}

	waitSec := cl.GetSpec().GetUpdate().GetWaitReadySeconds()
	if waitSec <= 0 {
		waitSec = 60
	}

	now := c.Now()
	stopReason := ""
	stopPhase := ""
	for _, m := range inflight {
		phase := m.view.GetPhase()
		if phase == pb.DeployPhase_DEPLOY_PHASE_FAILED || phase == pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK {
			stopPhase = "Failed"
			stopReason = fmt.Sprintf("machine %s phase %s", m.srv.GetMachine(), phase)
			break
		}
		key := inflightKey{cl.GetMetadata().GetName(), m.srv.GetMachine()}
		c.mu.Lock()
		dl, ok := c.deadlines[key]
		if !ok {
			dl = now.Add(time.Duration(waitSec) * time.Second)
			c.deadlines[key] = dl
		}
		c.mu.Unlock()
		if now.After(dl) && !view.ReadyConditionTrue(m.view) {
			stopPhase = "Degraded"
			stopReason = fmt.Sprintf("machine %s waitReadySeconds elapsed", m.srv.GetMachine())
			break
		}
	}
	if stopPhase != "" {
		return c.setStatus(cl, &pb.NatsClusterStatus{
			Phase:              stopPhase,
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "RollStopped",
			Message:            stopReason,
			Servers:            serverStatus,
		})
	}

	wrote := 0
	slots := int(maxUnavail) - len(inflight) - undeployed
	if slots < 0 || len(dropped) > undeployed {
		slots = 0
	}
	for _, m := range candidates {
		if wrote >= slots {
			break
		}
		detail := fmt.Sprintf("cluster=%s generation=%d", cl.GetMetadata().GetName(), cl.GetMetadata().GetGeneration())
		if _, _, err := c.Assign.Apply(ctx, assign.Request{
			MachineID:             m.srv.GetMachine(),
			Strategy:              strategy,
			Spec:                  m.computed,
			Action:                "NatsCluster",
			Detail:                detail,
			FromVersion:           m.live.GetArtifact().GetVersion(),
			ToVersion:             m.computed.GetArtifact().GetVersion(),
			EnforceLeaseInterlock: false,
			AllowReserved:         true,
		}); err != nil {
			return c.setAssignFailed(cl, serverStatus, err)
		}
		c.mu.Lock()
		c.deadlines[inflightKey{cl.GetMetadata().GetName(), m.srv.GetMachine()}] = now.Add(time.Duration(waitSec) * time.Second)
		c.mu.Unlock()
		wrote++
	}

	droppedLeft := len(c.droppedMachines(cl))
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
	return c.setStatus(cl, &pb.NatsClusterStatus{
		Phase:              phase,
		ObservedGeneration: obs,
		Servers:            serverStatus,
	})
}

func (c *Controller) reconcileDelete(ctx context.Context, cl *pb.NatsCluster) error {
	maxUnavail := cl.GetSpec().GetUpdate().GetMaxUnavailable()
	if maxUnavail < 1 {
		maxUnavail = 1
	}
	targets := c.assignedClusterMachines(cl)
	removed, err := c.undeployMachines(ctx, cl, targets, int(maxUnavail))
	if err != nil {
		return c.setAssignFailed(cl, cl.GetStatus().GetServers(), err)
	}
	if len(targets) > removed {
		return c.setStatus(cl, &pb.NatsClusterStatus{
			Phase:              "Deleting",
			Deleting:           true,
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "Undeploying",
			Servers:            cl.GetStatus().GetServers(),
		})
	}
	c.pruneClusterDeadlines(cl.GetMetadata().GetName())
	return c.Store.DeleteNatsCluster(cl.GetMetadata().GetName())
}

func (c *Controller) setAssignFailed(cl *pb.NatsCluster, servers []*pb.NatsServerStatus, err error) error {
	return c.setStatus(cl, &pb.NatsClusterStatus{
		Phase:              "Failed",
		ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
		Reason:             "AssignFailed",
		Message:            err.Error(),
		Servers:            servers,
	})
}

func (c *Controller) undeployMachines(ctx context.Context, cl *pb.NatsCluster, machines []string, limit int) (int, error) {
	strategy := store.ClusterStrategy(cl)
	removed := 0
	for _, machine := range machines {
		rec, ok := c.Store.GetMachine(machine)
		if !ok || rec.Assignments[strategy] == nil {
			c.clearDeadline(cl.GetMetadata().GetName(), machine)
			continue
		}
		if removed >= limit {
			continue
		}
		if _, _, err := c.Assign.Apply(ctx, assign.Request{
			MachineID:     machine,
			Strategy:      strategy,
			Spec:          nil,
			Action:        "NatsClusterUndeploy",
			Detail:        cl.GetMetadata().GetName(),
			AllowReserved: true,
		}); err != nil {
			return removed, err
		}
		c.clearDeadline(cl.GetMetadata().GetName(), machine)
		removed++
	}
	return removed, nil
}

func (c *Controller) droppedMachines(cl *pb.NatsCluster) []string {
	want := specMachines(cl)
	var out []string
	for _, id := range c.assignedClusterMachines(cl) {
		if _, ok := want[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func (c *Controller) assignedClusterMachines(cl *pb.NatsCluster) []string {
	strategy := store.ClusterStrategy(cl)
	var out []string
	for _, id := range c.ownedAssignmentMachines(cl) {
		rec, ok := c.Store.GetMachine(id)
		if !ok || rec.Assignments[strategy] == nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (c *Controller) ownedAssignmentMachines(cl *pb.NatsCluster) []string {
	strategy := store.ClusterStrategy(cl)
	name := cl.GetMetadata().GetName()
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, s := range cl.GetSpec().GetServers() {
		add(s.GetMachine())
	}
	for _, s := range cl.GetStatus().GetServers() {
		add(s.GetMachine())
	}
	for _, rec := range c.Store.ListMachines() {
		spec := rec.Assignments[strategy]
		if spec != nil && spec.GetEnv()["NATS_CLUSTER_NAME"] == name {
			add(rec.MachineID)
		}
	}
	sort.Strings(out)
	return out
}

func specMachines(cl *pb.NatsCluster) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range cl.GetSpec().GetServers() {
		out[s.GetMachine()] = struct{}{}
	}
	return out
}

func (c *Controller) clearDeadline(cluster, machine string) {
	c.mu.Lock()
	delete(c.deadlines, inflightKey{cluster, machine})
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

func (c *Controller) setStatus(cl *pb.NatsCluster, st *pb.NatsClusterStatus) error {
	if proto.Equal(cl.GetStatus(), st) {
		return nil
	}
	return c.Store.UpdateNatsClusterStatus(cl.GetMetadata().GetName(), st)
}

func computeAssignment(cl *pb.NatsCluster, self *pb.NatsServer, all []*pb.NatsServer, art, cfg *pb.ArtifactRef) *pb.StrategyAssignmentSpec {
	var routes []string
	for _, s := range all {
		if s.GetMachine() == self.GetMachine() {
			continue
		}
		routes = append(routes, fmt.Sprintf("nats://%s:%d", s.GetRouteHost(), natsPort(s.GetClusterPort(), 6222)))
	}
	sort.Strings(routes)
	var args []string
	if cfg != nil {
		args = append(args, "-c", "${CONFIG}")
	}
	if len(routes) > 0 {
		args = append(args, "--routes", strings.Join(routes, ","))
	}
	monitor := natsPort(self.GetMonitorPort(), 8222)
	health := cl.GetSpec().GetUpdate().GetWaitReadySeconds()
	if health <= 0 {
		health = 60
	}
	spec := &pb.StrategyAssignmentSpec{
		Strategy: store.ClusterStrategy(cl),
		Artifact: proto.Clone(art).(*pb.ArtifactRef),
		Stopped:  false,
		Args:     args,
		Env: map[string]string{
			"NATS_SERVER_NAME":  self.GetServerName(),
			"NATS_CLUSTER_NAME": cl.GetMetadata().GetName(),
			"NATS_CLIENT_PORT":  strconv.Itoa(natsPort(self.GetClientPort(), 4222)),
			"NATS_CLUSTER_PORT": strconv.Itoa(natsPort(self.GetClusterPort(), 6222)),
			"NATS_MONITOR_PORT": strconv.Itoa(monitor),
		},
		DeployPolicy: &pb.DeployPolicy{
			Startsecs:           2,
			HealthWindowSeconds: health,
			MaxCrashesInWindow:  3,
			StopGraceSeconds:    10,
			EnableAutoRollback:  true,
		},
		Readiness: &pb.ReadinessProbe{
			Endpoint: fmt.Sprintf("http://127.0.0.1:%d/healthz", monitor),
		},
	}
	if cfg != nil {
		spec.Config = proto.Clone(cfg).(*pb.ArtifactRef)
	}
	if art.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE {
		spec.Driver = pb.ExecutionDriver_EXECUTION_DRIVER_OCI
	} else {
		spec.Driver = pb.ExecutionDriver_EXECUTION_DRIVER_EXEC
	}
	return spec
}

func natsPort(got, def int32) int {
	if got <= 0 {
		return int(def)
	}
	return int(got)
}

func resolveClusterArtifacts(st store.Store, cl *pb.NatsCluster) (art, cfg *pb.ArtifactRef, err error) {
	strategy := store.ClusterStrategy(cl)
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
