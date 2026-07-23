// Package api implements the human-facing ControlPlaneService over Connect:
// unary reads/writes and WatchMachine server-streaming.
// It is a different connection from AgentService — low-frequency, browser/CLI,
// curl-able HTTP/JSON — and shares the same store + DesiredState push path.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/filebrowse"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/buildinfo"
	"github.com/bullionbear/strategon/internal/controlplane/filetransfer"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/sharedfile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MinFileBrowseAgentVersion is the capability version that advertises
// ListDir/FetchFiles support.
const MinFileBrowseAgentVersion int32 = 2

// AgentNotifier pushes a fresh DesiredState to a connected agent after a write.
type AgentNotifier interface {
	Notify(machineID string)
}

// AgentController extends AgentNotifier with imperative southbound sends.
type AgentController interface {
	AgentNotifier
	SendControl(ctx context.Context, machineID string, msg *pb.ControlMessage) error
}

// Server implements strategyplatformv1connect.ControlPlaneServiceHandler.
type Server struct {
	store  store.Store
	hub    *store.Hub
	agents AgentNotifier
	broker *filetransfer.Broker
	logger *slog.Logger
}

// New constructs a human-API server.
func New(st store.Store, hub *store.Hub, agents AgentNotifier, logger *slog.Logger) *Server {
	return NewWithBroker(st, hub, agents, nil, logger)
}

// NewWithBroker constructs a human-API server with file-transfer correlation.
func NewWithBroker(st store.Store, hub *store.Hub, agents AgentNotifier, broker *filetransfer.Broker, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: st, hub: hub, agents: agents, broker: broker, logger: logger}
}

func (s *Server) ListMachines(_ context.Context, req *connect.Request[pb.ListMachinesRequest]) (*connect.Response[pb.ListMachinesResponse], error) {
	recs := s.store.ListMachines()
	machines := make([]*pb.Machine, 0, len(recs))
	for _, rec := range recs {
		machines = append(machines, BuildMachine(rec, s.store))
	}
	// page_token is a stub; in-memory fleet sizes are tiny.
	_ = req
	return connect.NewResponse(&pb.ListMachinesResponse{Machines: machines}), nil
}

func (s *Server) GetMachine(_ context.Context, req *connect.Request[pb.GetMachineRequest]) (*connect.Response[pb.Machine], error) {
	rec, ok := s.store.GetMachine(req.Msg.GetMachineId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", req.Msg.GetMachineId()))
	}
	return connect.NewResponse(BuildMachine(rec, s.store)), nil
}

func (s *Server) GetControlPlaneVersion(_ context.Context, _ *connect.Request[pb.GetControlPlaneVersionRequest]) (*connect.Response[pb.ControlPlaneVersion], error) {
	return connect.NewResponse(&pb.ControlPlaneVersion{
		Version:    buildinfo.Version,
		CommitHash: buildinfo.CommitHash,
		BuildTime:  buildinfo.BuildTime,
	}), nil
}

func (s *Server) GetMachineMetrics(_ context.Context, req *connect.Request[pb.GetMachineMetricsRequest]) (*connect.Response[pb.GetMachineMetricsResponse], error) {
	machineID := req.Msg.GetMachineId()
	if machineID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	if _, ok := s.store.GetMachine(machineID); !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}
	rangeSec := req.Msg.GetRangeSeconds()
	if rangeSec <= 0 {
		rangeSec = int64(store.ResourceSampleRetain / time.Second)
	}
	since := time.Now().UTC().Add(-time.Duration(rangeSec) * time.Second)
	samples, err := s.store.ListResourceSamples(machineID, req.Msg.GetStrategy(), since)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Downsample to ~60 points for sparkline payloads.
	samples = downsampleSamples(samples, 60)
	out := make([]*pb.ResourceSamplePoint, 0, len(samples))
	for _, s := range samples {
		out = append(out, &pb.ResourceSamplePoint{
			SampledAt:  timestamppb.New(s.SampledAt),
			CpuPercent: s.CPUPercent,
			MemBytes:   s.MemBytes,
		})
	}
	return connect.NewResponse(&pb.GetMachineMetricsResponse{Samples: out}), nil
}

func downsampleSamples(in []store.ResourceSample, max int) []store.ResourceSample {
	if max <= 0 || len(in) <= max {
		return in
	}
	out := make([]store.ResourceSample, 0, max)
	for i := 0; i < max; i++ {
		idx := i * (len(in) - 1) / (max - 1)
		out = append(out, in[idx])
	}
	return out
}

func (s *Server) Deploy(ctx context.Context, req *connect.Request[pb.DeployRequest]) (*connect.Response[pb.DeployResponse], error) {
	msg := req.Msg
	spec, art, fromVersion, err := s.buildDeploymentSpec(msg.GetMachineId(), msg.GetStrategy(), msg.GetArtifactVersion(), msg.GetConfigVersion(), nil, nil, false)
	if err != nil {
		return nil, err
	}
	gen, err := s.commitAssignment(ctx, msg.GetMachineId(), msg.GetStrategy(), spec, "Deploy", fromVersion, art.GetVersion())
	if err != nil {
		return nil, err
	}
	s.logger.Info("deploy", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"version", art.GetVersion(), "generation", gen, "actor", auth.ActorFromContext(ctx))
	return connect.NewResponse(&pb.DeployResponse{Generation: gen}), nil
}

func (s *Server) SetDeployment(ctx context.Context, req *connect.Request[pb.SetDeploymentRequest]) (*connect.Response[pb.SetDeploymentResponse], error) {
	msg := req.Msg
	spec, art, fromVersion, err := s.buildDeploymentSpec(msg.GetMachineId(), msg.GetStrategy(), msg.GetArtifactVersion(), msg.GetConfigVersion(), msg.GetArgs(), msg.GetEnv(), true)
	if err != nil {
		return nil, err
	}
	gen, err := s.commitAssignment(ctx, msg.GetMachineId(), msg.GetStrategy(), spec, "SetDeployment", fromVersion, art.GetVersion())
	if err != nil {
		return nil, err
	}
	s.logger.Info("set_deployment", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"version", art.GetVersion(), "generation", gen, "actor", auth.ActorFromContext(ctx))
	return connect.NewResponse(&pb.SetDeploymentResponse{Generation: gen}), nil
}

// buildDeploymentSpec resolves artifact/config and assembles an assignment.
// When setRuntime is true, args/env are replaced from the request (SetDeployment);
// otherwise they are preserved from the existing assignment (Deploy).
//
// artifactVersion / configVersion may be the sentinel "latest": the control
// plane resolves it to the newest registered version (by created_at) and
// stores that concrete version in the deployment — never the string "latest".
func (s *Server) buildDeploymentSpec(machineID, strategy, artifactVersion, configVersion string, args []string, env map[string]string, setRuntime bool) (*pb.StrategyAssignmentSpec, *pb.ArtifactRef, string, error) {
	if machineID == "" || strategy == "" || artifactVersion == "" {
		return nil, nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id, strategy, and artifact_version are required"))
	}
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return nil, nil, "", connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}

	existing := rec.Assignments[strategy]
	artName := strategy
	if existing != nil && existing.GetArtifact().GetName() != "" {
		artName = existing.GetArtifact().GetName()
	}
	art, err := s.resolveArtifact(artName, strategy, artifactVersion)
	if err != nil {
		return nil, nil, "", err
	}

	spec := defaultOrCloneSpec(existing, strategy)
	fromVersion := spec.GetArtifact().GetVersion()
	spec.Artifact = art

	if configVersion != "" {
		cfg, err := s.resolveArtifact(art.GetName()+"-config", strategy+"-config", configVersion)
		if err != nil {
			return nil, nil, "", err
		}
		spec.Config = cfg
	}

	if setRuntime {
		spec.Args = append([]string(nil), args...)
		if env == nil {
			spec.Env = nil
		} else {
			spec.Env = make(map[string]string, len(env))
			for k, v := range env {
				spec.Env[k] = v
			}
		}
	}

	// Create-then-start: a brand-new assignment lands halted; updates preserve
	// the existing run state (redeploy while stopped stays stopped).
	if existing == nil {
		spec.Stopped = true
	}

	if blocked, reason := store.DeploymentBlockedByLease(s.store, machineID, strategy); blocked {
		return nil, nil, "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("migration interlocking: lease for %q %s", strategy, reason))
	}
	return spec, art, fromVersion, nil
}

// resolveArtifact looks up name/version, trying primary then fallback name.
// version "latest" selects the newest registered artifact by created_at and
// returns that concrete ArtifactRef (deployment never stores "latest").
func (s *Server) resolveArtifact(primaryName, fallbackName, version string) (*pb.ArtifactRef, error) {
	if version == "latest" {
		art, ok := latestArtifact(s.store, primaryName)
		if !ok && fallbackName != "" && fallbackName != primaryName {
			art, ok = latestArtifact(s.store, fallbackName)
		}
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("artifact %q has no registered versions; call RegisterArtifact first", primaryName))
		}
		return art, nil
	}
	art, ok := s.store.GetArtifact(primaryName, version)
	if !ok && fallbackName != "" && fallbackName != primaryName {
		art, ok = s.store.GetArtifact(fallbackName, version)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("artifact %q version %q not registered; call RegisterArtifact first", primaryName, version))
	}
	return art, nil
}

func latestArtifact(st store.Store, name string) (*pb.ArtifactRef, bool) {
	list := st.ListArtifacts(name)
	if len(list) == 0 {
		return nil, false
	}
	// ListArtifacts returns newest-first within a name.
	return list[0], true
}

func (s *Server) commitAssignment(ctx context.Context, machineID, strategy string, spec *pb.StrategyAssignmentSpec, action, fromVersion, toVersion string) (int64, error) {
	gen, err := s.store.SetAssignment(machineID, strategy, spec)
	if err != nil {
		return 0, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       auth.ActorFromContext(ctx),
		Action:      action,
		MachineId:   machineID,
		Strategy:    strategy,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
	})
	if s.agents != nil {
		s.agents.Notify(machineID)
	}
	return gen, nil
}

func (s *Server) Rollback(ctx context.Context, req *connect.Request[pb.RollbackRequest]) (*connect.Response[pb.RollbackResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" || msg.GetStrategy() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id and strategy are required"))
	}
	rec, ok := s.store.GetMachine(msg.GetMachineId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	spec := rec.Assignments[msg.GetStrategy()]
	if spec == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("strategy %q not assigned", msg.GetStrategy()))
	}
	fromVersion := spec.GetArtifact().GetVersion()

	var target *pb.ArtifactRef
	if tv := msg.GetTargetVersion(); tv != "" {
		name := spec.GetArtifact().GetName()
		if name == "" {
			name = msg.GetStrategy()
		}
		art, ok := s.store.GetArtifact(name, tv)
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("artifact version %q not registered", tv))
		}
		target = art
	} else {
		prev, ok := s.store.PreviousArtifact(msg.GetMachineId(), msg.GetStrategy())
		if !ok {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no previous version to roll back to"))
		}
		target = prev
	}

	next := proto.Clone(spec).(*pb.StrategyAssignmentSpec)
	next.Artifact = target
	gen, err := s.store.SetAssignment(msg.GetMachineId(), msg.GetStrategy(), next)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       auth.ActorFromContext(ctx),
		Action:      "Rollback",
		MachineId:   msg.GetMachineId(),
		Strategy:    msg.GetStrategy(),
		FromVersion: fromVersion,
		ToVersion:   target.GetVersion(),
	})
	if s.agents != nil {
		s.agents.Notify(msg.GetMachineId())
	}
	return connect.NewResponse(&pb.RollbackResponse{Generation: gen}), nil
}

func (s *Server) Stop(ctx context.Context, req *connect.Request[pb.StopRequest]) (*connect.Response[pb.StopResponse], error) {
	gen, err := s.setRunState(ctx, req.Msg.GetMachineId(), req.Msg.GetStrategy(), true, "Stop")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.StopResponse{Generation: gen}), nil
}

func (s *Server) Start(ctx context.Context, req *connect.Request[pb.StartRequest]) (*connect.Response[pb.StartResponse], error) {
	gen, err := s.setRunState(ctx, req.Msg.GetMachineId(), req.Msg.GetStrategy(), false, "Start")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.StartResponse{Generation: gen}), nil
}

// setRunState flips StrategyAssignmentSpec.stopped without touching
// artifact/config/args/env. Idempotent when already in the target state.
func (s *Server) setRunState(ctx context.Context, machineID, strategy string, stopped bool, action string) (int64, error) {
	if machineID == "" || strategy == "" {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id and strategy are required"))
	}
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return 0, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}
	spec := rec.Assignments[strategy]
	if spec == nil {
		return 0, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("strategy %q not assigned", strategy))
	}
	if spec.GetStopped() == stopped {
		return rec.Generation, nil
	}
	next := proto.Clone(spec).(*pb.StrategyAssignmentSpec)
	next.Stopped = stopped
	gen, err := s.store.SetAssignment(machineID, strategy, next)
	if err != nil {
		return 0, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       auth.ActorFromContext(ctx),
		Action:      action,
		MachineId:   machineID,
		Strategy:    strategy,
		FromVersion: spec.GetArtifact().GetVersion(),
		ToVersion:   spec.GetArtifact().GetVersion(),
	})
	if s.agents != nil {
		s.agents.Notify(machineID)
	}
	s.logger.Info(strings.ToLower(action), "machine_id", machineID, "strategy", strategy,
		"stopped", stopped, "generation", gen, "actor", auth.ActorFromContext(ctx))
	return gen, nil
}

func (s *Server) Undeploy(ctx context.Context, req *connect.Request[pb.UndeployRequest]) (*connect.Response[pb.UndeployResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" || msg.GetStrategy() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id and strategy are required"))
	}
	rec, ok := s.store.GetMachine(msg.GetMachineId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	spec := rec.Assignments[msg.GetStrategy()]
	if spec == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("strategy %q not assigned", msg.GetStrategy()))
	}
	fromVersion := spec.GetArtifact().GetVersion()

	gen, err := s.store.SetAssignment(msg.GetMachineId(), msg.GetStrategy(), nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       auth.ActorFromContext(ctx),
		Action:      "Undeploy",
		MachineId:   msg.GetMachineId(),
		Strategy:    msg.GetStrategy(),
		FromVersion: fromVersion,
	})
	if s.agents != nil {
		s.agents.Notify(msg.GetMachineId())
	}
	s.logger.Info("undeploy", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"from_version", fromVersion, "generation", gen, "actor", auth.ActorFromContext(ctx))
	return connect.NewResponse(&pb.UndeployResponse{Generation: gen}), nil
}

func (s *Server) SetSchedule(ctx context.Context, req *connect.Request[pb.SetScheduleRequest]) (*connect.Response[pb.SetScheduleResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" || msg.GetStrategy() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id and strategy are required"))
	}
	if err := validateSchedules(msg.GetSchedules()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	rec, ok := s.store.GetMachine(msg.GetMachineId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	spec := rec.Assignments[msg.GetStrategy()]
	if spec == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("strategy %q not assigned", msg.GetStrategy()))
	}
	next := proto.Clone(spec).(*pb.StrategyAssignmentSpec)
	next.Schedules = msg.GetSchedules()
	gen, err := s.store.SetAssignment(msg.GetMachineId(), msg.GetStrategy(), next)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp: timestamppb.Now(),
		Actor:     auth.ActorFromContext(ctx),
		Action:    "ConfigChange",
		MachineId: msg.GetMachineId(),
		Strategy:  msg.GetStrategy(),
	})
	if s.agents != nil {
		s.agents.Notify(msg.GetMachineId())
	}
	return connect.NewResponse(&pb.SetScheduleResponse{Generation: gen}), nil
}

func (s *Server) SetSharedFiles(ctx context.Context, req *connect.Request[pb.SetSharedFilesRequest]) (*connect.Response[pb.SetSharedFilesResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	if _, ok := s.store.GetMachine(msg.GetMachineId()); !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	seen := make(map[string]struct{}, len(msg.GetFiles()))
	specs := make([]*pb.SharedFileSpec, 0, len(msg.GetFiles()))
	for _, f := range msg.GetFiles() {
		if f == nil {
			continue
		}
		if err := sharedfile.ValidateName(f.GetName()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if f.GetArtifactVersion() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("shared file %q: artifact_version is required", f.GetName()))
		}
		if _, dup := seen[f.GetName()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("duplicate shared file name %q", f.GetName()))
		}
		seen[f.GetName()] = struct{}{}
		artName := f.GetArtifactName()
		if artName == "" {
			artName = f.GetName()
		}
		art, ok := s.store.GetArtifact(artName, f.GetArtifactVersion())
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("artifact %q version %q not registered", artName, f.GetArtifactVersion()))
		}
		if err := requireSHA256Digest(art.GetDigest()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("shared file %q: %w", f.GetName(), err))
		}
		specs = append(specs, &pb.SharedFileSpec{
			Name:     f.GetName(),
			Artifact: art,
		})
	}
	sharedGen, _, changed, err := s.store.SetSharedFiles(msg.GetMachineId(), specs)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if changed {
		names := make([]string, 0, len(specs))
		for _, sp := range specs {
			names = append(names, sp.GetName())
		}
		_ = s.store.AppendAudit(&pb.AuditEntry{
			Timestamp: timestamppb.Now(),
			Actor:     auth.ActorFromContext(ctx),
			Action:    "SetSharedFiles",
			MachineId: msg.GetMachineId(),
			Detail:    strings.Join(names, ","),
		})
		if s.agents != nil {
			s.agents.Notify(msg.GetMachineId())
		}
	}
	return connect.NewResponse(&pb.SetSharedFilesResponse{Generation: sharedGen}), nil
}

func (s *Server) ListSharedFiles(_ context.Context, req *connect.Request[pb.ListSharedFilesRequest]) (*connect.Response[pb.ListSharedFilesResponse], error) {
	machineID := req.Msg.GetMachineId()
	if machineID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}
	running := map[string]*pb.SharedFileStatus{}
	if rec.SharedStatus != nil {
		for _, f := range rec.SharedStatus.GetFiles() {
			running[f.GetName()] = f
		}
	}
	// Union desired + status so agent-reported removal failures stay visible.
	names := make([]string, 0, len(rec.SharedFiles)+len(running))
	seen := map[string]struct{}{}
	for n := range rec.SharedFiles {
		seen[n] = struct{}{}
		names = append(names, n)
	}
	for n := range running {
		if _, ok := seen[n]; ok {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	views := make([]*pb.SharedFileView, 0, len(names))
	for _, n := range names {
		spec := rec.SharedFiles[n]
		st := running[n]
		desiredDigest := ""
		desiredVersion := ""
		if spec != nil && spec.GetArtifact() != nil {
			desiredDigest = spec.GetArtifact().GetDigest()
			desiredVersion = spec.GetArtifact().GetVersion()
		}
		runningDigest := ""
		lastErr := ""
		if st != nil {
			runningDigest = st.GetRunningDigest()
			lastErr = st.GetLastError()
		}
		views = append(views, &pb.SharedFileView{
			Name:           n,
			DesiredVersion: desiredVersion,
			DesiredDigest:  desiredDigest,
			RunningDigest:  runningDigest,
			Converged:      desiredDigest != "" && desiredDigest == runningDigest && lastErr == "",
			LastError:      lastErr,
		})
	}
	return connect.NewResponse(&pb.ListSharedFilesResponse{Files: views}), nil
}

func (s *Server) WatchMachine(ctx context.Context, req *connect.Request[pb.GetMachineRequest], stream *connect.ServerStream[pb.MachineStatusEvent]) error {
	machineID := req.Msg.GetMachineId()
	if machineID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	if _, ok := s.store.GetMachine(machineID); !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}

	// Initial full snapshot — same mental model as agent resync.
	if err := s.sendMachineEvent(stream, machineID); err != nil {
		return err
	}

	var notify <-chan struct{}
	var cancel func()
	if s.hub != nil {
		notify, cancel = s.hub.Subscribe(machineID)
		defer cancel()
	} else {
		// Degenerate: poll every second if no hub (shouldn't happen in production wiring).
		ch := make(chan struct{})
		notify = ch
		cancel = func() {}
		defer cancel()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-notify:
			if err := s.sendMachineEvent(stream, machineID); err != nil {
				return err
			}
		}
	}
}

func (s *Server) sendMachineEvent(stream *connect.ServerStream[pb.MachineStatusEvent], machineID string) error {
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}
	return stream.Send(&pb.MachineStatusEvent{
		MachineId: machineID,
		Machine:   BuildMachine(rec, s.store),
		At:        timestamppb.Now(),
	})
}

func (s *Server) ListAudit(_ context.Context, req *connect.Request[pb.ListAuditRequest]) (*connect.Response[pb.ListAuditResponse], error) {
	entries := s.store.ListAudit(req.Msg.GetMachineId(), req.Msg.GetStrategy())
	if n := int(req.Msg.GetPageSize()); n > 0 && n < len(entries) {
		entries = entries[:n]
	}
	return connect.NewResponse(&pb.ListAuditResponse{Entries: entries}), nil
}

func (s *Server) RegisterArtifact(ctx context.Context, req *connect.Request[pb.RegisterArtifactRequest]) (*connect.Response[pb.RegisterArtifactResponse], error) {
	art := req.Msg.GetArtifact()
	if err := requireSHA256Digest(art.GetDigest()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.store.RegisterArtifact(art); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp: timestamppb.Now(),
		Actor:     auth.ActorFromContext(ctx),
		Action:    "RegisterArtifact",
		Strategy:  art.GetName(),
		ToVersion: art.GetVersion(),
	})
	return connect.NewResponse(&pb.RegisterArtifactResponse{}), nil
}

func (s *Server) ListArtifacts(_ context.Context, req *connect.Request[pb.ListArtifactsRequest]) (*connect.Response[pb.ListArtifactsResponse], error) {
	arts := s.store.ListArtifacts(req.Msg.GetName())
	return connect.NewResponse(&pb.ListArtifactsResponse{Artifacts: arts}), nil
}

// requireSHA256Digest enforces the only digest algorithm the agent store
// round-trips today (sha256: + hex). Other prefixes would disagree between
// digestDirName and RunningSharedDigest.
func requireSHA256Digest(digest string) error {
	d := strings.TrimSpace(digest)
	if d == "" {
		return errors.New("digest is required")
	}
	lower := strings.ToLower(d)
	if !strings.HasPrefix(lower, "sha256:") {
		return fmt.Errorf("digest must use sha256: prefix, got %q", digest)
	}
	hexPart := d[len("sha256:"):]
	if hexPart == "" {
		return errors.New("sha256 digest is empty")
	}
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return fmt.Errorf("sha256 digest has non-hex character")
		}
	}
	return nil
}

func defaultOrCloneSpec(existing *pb.StrategyAssignmentSpec, strategy string) *pb.StrategyAssignmentSpec {
	if existing != nil {
		return proto.Clone(existing).(*pb.StrategyAssignmentSpec)
	}
	return &pb.StrategyAssignmentSpec{
		Strategy: strategy,
		Driver:   pb.ExecutionDriver_EXECUTION_DRIVER_EXEC,
		DeployPolicy: &pb.DeployPolicy{
			Startsecs:           2,
			HealthWindowSeconds: 30,
			MaxCrashesInWindow:  3,
			StopGraceSeconds:    10,
			EnableAutoRollback:  true,
		},
	}
}

func (s *Server) BrowseDir(ctx context.Context, req *connect.Request[pb.BrowseDirRequest]) (*connect.Response[pb.BrowseDirResponse], error) {
	msg := req.Msg
	if err := s.gateFileBrowse(msg.GetMachineId(), msg.GetStrategy()); err != nil {
		return nil, err
	}
	ctrl, err := s.agentControl()
	if err != nil {
		return nil, err
	}
	if s.broker == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("file transfer broker not configured"))
	}

	reqID, listingCh, cancel := s.broker.NewListing(msg.GetMachineId())
	defer cancel()

	if err := ctrl.SendControl(ctx, msg.GetMachineId(), &pb.ControlMessage{
		MessageId: reqID,
		Payload: &pb.ControlMessage_ListDir{ListDir: &pb.ListDir{
			RequestId: reqID,
			Strategy:  msg.GetStrategy(),
			Path:      msg.GetPath(),
		}},
	}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(filetransfer.BrowseTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("browse timed out waiting for agent"))
	case listing := <-listingCh:
		if listing.GetError() != "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(listing.GetError()))
		}
		return connect.NewResponse(&pb.BrowseDirResponse{
			Entries: listing.GetEntries(),
			Path:    listing.GetPath(),
		}), nil
	}
}

func (s *Server) DownloadFiles(ctx context.Context, req *connect.Request[pb.DownloadFilesRequest], stream *connect.ServerStream[pb.DownloadChunk]) error {
	msg := req.Msg
	if err := s.gateFileBrowse(msg.GetMachineId(), msg.GetStrategy()); err != nil {
		return err
	}
	paths := msg.GetPaths()
	if len(paths) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("paths is required"))
	}
	if len(paths) > filebrowse.MaxTarballFiles {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("at most %d paths allowed", filebrowse.MaxTarballFiles))
	}
	ctrl, err := s.agentControl()
	if err != nil {
		return err
	}
	if s.broker == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("file transfer broker not configured"))
	}

	reqID, chunkCh, cancel := s.broker.NewDownload(msg.GetMachineId())
	defer cancel()

	if err := ctrl.SendControl(ctx, msg.GetMachineId(), &pb.ControlMessage{
		MessageId: reqID,
		Payload: &pb.ControlMessage_FetchFiles{FetchFiles: &pb.FetchFiles{
			RequestId: reqID,
			Strategy:  msg.GetStrategy(),
			Paths:     paths,
		}},
	}); err != nil {
		return err
	}

	deadline := time.NewTimer(filetransfer.DownloadTimeout)
	defer deadline.Stop()

	var filename string
	var kind pb.TransferKind
	var bytesSent int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("download timed out waiting for agent"))
		case chunk, ok := <-chunkCh:
			if !ok {
				return connect.NewError(connect.CodeAborted, errors.New("download cancelled"))
			}
			if chunk.GetError() != "" {
				return connect.NewError(connect.CodeFailedPrecondition, errors.New(chunk.GetError()))
			}
			if chunk.GetFilename() != "" {
				filename = chunk.GetFilename()
			}
			if chunk.GetTransferKind() != pb.TransferKind_TRANSFER_KIND_UNSPECIFIED {
				kind = chunk.GetTransferKind()
			}
			bytesSent += int64(len(chunk.GetData()))
			out := &pb.DownloadChunk{
				Data:         chunk.GetData(),
				Filename:     filename,
				TransferKind: kind,
				Eof:          chunk.GetEof(),
			}
			if err := stream.Send(out); err != nil {
				return err
			}
			if chunk.GetEof() {
				// Audit only after a successful delivery so failed agent
				// validation (rejected paths, caps, etc.) is not logged as a download.
				_ = s.store.AppendAudit(&pb.AuditEntry{
					Timestamp: timestamppb.Now(),
					Actor:     auth.ActorFromContext(ctx),
					Action:    "DownloadFiles",
					MachineId: msg.GetMachineId(),
					Strategy:  msg.GetStrategy(),
					Detail: fmt.Sprintf("paths=%s\nfilename=%s\nkind=%s\nbytes=%d",
						strings.Join(paths, ","), filename, kind.String(), bytesSent),
				})
				return nil
			}
		}
	}
}

func (s *Server) gateFileBrowse(machineID, strategy string) error {
	if machineID == "" || strategy == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id and strategy are required"))
	}
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}
	if !rec.Reachable {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %q is not reachable", machineID))
	}
	if rec.AgentVersion < MinFileBrowseAgentVersion {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("machine %q agent_version %d does not support file browse (need >= %d)",
				machineID, rec.AgentVersion, MinFileBrowseAgentVersion))
	}
	return nil
}

func (s *Server) agentControl() (AgentController, error) {
	ctrl, ok := s.agents.(AgentController)
	if !ok || s.agents == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("agent control not available"))
	}
	return ctrl, nil
}
