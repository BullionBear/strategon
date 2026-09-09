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
		}
	}

	maxUnavail := cl.GetSpec().GetUpdate().GetMaxUnavailable()
	if maxUnavail < 1 {
		maxUnavail = 1
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
	slots := int(maxUnavail) - len(inflight)
	if slots < 0 {
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
			return err
		}
		c.mu.Lock()
		c.deadlines[inflightKey{cl.GetMetadata().GetName(), m.srv.GetMachine()}] = now.Add(time.Duration(waitSec) * time.Second)
		c.mu.Unlock()
		wrote++
	}

	allReady := len(candidates) == 0 && len(inflight) == 0 && len(members) > 0
	phase := "Rolling"
	obs := cl.GetStatus().GetObservedGeneration()
	if allReady {
		phase = "Ready"
		obs = cl.GetMetadata().GetGeneration()
		c.mu.Lock()
		for k := range c.deadlines {
			if k.cluster == cl.GetMetadata().GetName() {
				delete(c.deadlines, k)
			}
		}
		c.mu.Unlock()
	} else if len(members) == 0 {
		phase = "Pending"
	} else if wrote == 0 && len(inflight) == 0 && len(candidates) > 0 {
		phase = "Pending"
	}
	return c.setStatus(cl, &pb.NatsClusterStatus{
		Phase:              phase,
		ObservedGeneration: obs,
		Servers:            serverStatus,
	})
}

func (c *Controller) reconcileDelete(ctx context.Context, cl *pb.NatsCluster) error {
	strategy := store.ClusterStrategy(cl)
	maxUnavail := cl.GetSpec().GetUpdate().GetMaxUnavailable()
	if maxUnavail < 1 {
		maxUnavail = 1
	}
	remaining := 0
	removed := 0
	for _, srv := range cl.GetSpec().GetServers() {
		rec, ok := c.Store.GetMachine(srv.GetMachine())
		if !ok || rec.Assignments[strategy] == nil {
			continue
		}
		remaining++
		if removed >= int(maxUnavail) {
			continue
		}
		if _, _, err := c.Assign.Apply(ctx, assign.Request{
			MachineID:     srv.GetMachine(),
			Strategy:      strategy,
			Spec:          nil,
			Action:        "NatsClusterUndeploy",
			Detail:        cl.GetMetadata().GetName(),
			AllowReserved: true,
		}); err != nil {
			return err
		}
		removed++
	}
	if remaining-removed > 0 {
		return c.setStatus(cl, &pb.NatsClusterStatus{
			Phase:              "Deleting",
			Deleting:           true,
			ObservedGeneration: cl.GetStatus().GetObservedGeneration(),
			Reason:             "Undeploying",
		})
	}
	return c.Store.DeleteNatsCluster(cl.GetMetadata().GetName())
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
	args := []string{"-c", "${CONFIG}"}
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
