// Package api implements the human-facing ControlPlaneService over Connect
// (FRONTEND.md): unary reads/writes and WatchMachine server-streaming.
// It is a different connection from AgentService — low-frequency, browser/CLI,
// curl-able HTTP/JSON — and shares the same store + DesiredState push path.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/buildinfo"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AgentNotifier pushes a fresh DesiredState to a connected agent after a write.
type AgentNotifier interface {
	Notify(machineID string)
}

// Server implements strategyplatformv1connect.ControlPlaneServiceHandler.
type Server struct {
	store  store.Store
	hub    *store.Hub
	agents AgentNotifier
	logger *slog.Logger
}

// New constructs a human-API server.
func New(st store.Store, hub *store.Hub, agents AgentNotifier, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: st, hub: hub, agents: agents, logger: logger}
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

func (s *Server) Deploy(_ context.Context, req *connect.Request[pb.DeployRequest]) (*connect.Response[pb.DeployResponse], error) {
	msg := req.Msg
	spec, art, fromVersion, err := s.buildDeploymentSpec(msg.GetMachineId(), msg.GetStrategy(), msg.GetArtifactVersion(), msg.GetConfigVersion(), nil, nil, false)
	if err != nil {
		return nil, err
	}
	gen, err := s.commitAssignment(msg.GetMachineId(), msg.GetStrategy(), spec, "Deploy", fromVersion, art.GetVersion())
	if err != nil {
		return nil, err
	}
	s.logger.Info("deploy", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"version", art.GetVersion(), "generation", gen)
	return connect.NewResponse(&pb.DeployResponse{Generation: gen}), nil
}

func (s *Server) SetDeployment(_ context.Context, req *connect.Request[pb.SetDeploymentRequest]) (*connect.Response[pb.SetDeploymentResponse], error) {
	msg := req.Msg
	spec, art, fromVersion, err := s.buildDeploymentSpec(msg.GetMachineId(), msg.GetStrategy(), msg.GetArtifactVersion(), msg.GetConfigVersion(), msg.GetArgs(), msg.GetEnv(), true)
	if err != nil {
		return nil, err
	}
	gen, err := s.commitAssignment(msg.GetMachineId(), msg.GetStrategy(), spec, "SetDeployment", fromVersion, art.GetVersion())
	if err != nil {
		return nil, err
	}
	s.logger.Info("set_deployment", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"version", art.GetVersion(), "generation", gen)
	return connect.NewResponse(&pb.SetDeploymentResponse{Generation: gen}), nil
}

// buildDeploymentSpec resolves artifact/config and assembles an assignment.
// When setRuntime is true, args/env are replaced from the request (SetDeployment);
// otherwise they are preserved from the existing assignment (Deploy).
func (s *Server) buildDeploymentSpec(machineID, strategy, artifactVersion, configVersion string, args []string, env map[string]string, setRuntime bool) (*pb.StrategyAssignmentSpec, *pb.ArtifactRef, string, error) {
	if machineID == "" || strategy == "" || artifactVersion == "" {
		return nil, nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id, strategy, and artifact_version are required"))
	}
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return nil, nil, "", connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}

	artName := strategy
	if existing := rec.Assignments[strategy]; existing != nil && existing.GetArtifact().GetName() != "" {
		artName = existing.GetArtifact().GetName()
	}
	art, ok := s.store.GetArtifact(artName, artifactVersion)
	if !ok {
		art, ok = s.store.GetArtifact(strategy, artifactVersion)
	}
	if !ok {
		return nil, nil, "", connect.NewError(connect.CodeNotFound,
			fmt.Errorf("artifact %q version %q not registered; call RegisterArtifact first", artName, artifactVersion))
	}

	spec := defaultOrCloneSpec(rec.Assignments[strategy], strategy)
	fromVersion := spec.GetArtifact().GetVersion()
	spec.Artifact = art

	if configVersion != "" {
		cfg, ok := s.store.GetArtifact(art.GetName()+"-config", configVersion)
		if !ok {
			cfg, ok = s.store.GetArtifact(strategy+"-config", configVersion)
		}
		if !ok {
			return nil, nil, "", connect.NewError(connect.CodeNotFound, fmt.Errorf("config version %q not registered", configVersion))
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

	if blocked, reason := store.DeploymentBlockedByLease(s.store, machineID, strategy); blocked {
		return nil, nil, "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("migration interlocking: lease for %q %s", strategy, reason))
	}
	return spec, art, fromVersion, nil
}

func (s *Server) commitAssignment(machineID, strategy string, spec *pb.StrategyAssignmentSpec, action, fromVersion, toVersion string) (int64, error) {
	gen, err := s.store.SetAssignment(machineID, strategy, spec)
	if err != nil {
		return 0, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       "local",
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

func (s *Server) Rollback(_ context.Context, req *connect.Request[pb.RollbackRequest]) (*connect.Response[pb.RollbackResponse], error) {
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
		Actor:       "local",
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

func (s *Server) Undeploy(_ context.Context, req *connect.Request[pb.UndeployRequest]) (*connect.Response[pb.UndeployResponse], error) {
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
		Actor:       "local",
		Action:      "Undeploy",
		MachineId:   msg.GetMachineId(),
		Strategy:    msg.GetStrategy(),
		FromVersion: fromVersion,
	})
	if s.agents != nil {
		s.agents.Notify(msg.GetMachineId())
	}
	s.logger.Info("undeploy", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"from_version", fromVersion, "generation", gen)
	return connect.NewResponse(&pb.UndeployResponse{Generation: gen}), nil
}

func (s *Server) SetSchedule(_ context.Context, req *connect.Request[pb.SetScheduleRequest]) (*connect.Response[pb.SetScheduleResponse], error) {
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
		Actor:     "local",
		Action:    "ConfigChange",
		MachineId: msg.GetMachineId(),
		Strategy:  msg.GetStrategy(),
	})
	if s.agents != nil {
		s.agents.Notify(msg.GetMachineId())
	}
	return connect.NewResponse(&pb.SetScheduleResponse{Generation: gen}), nil
}

func (s *Server) WatchMachine(ctx context.Context, req *connect.Request[pb.GetMachineRequest], stream *connect.ServerStream[pb.MachineStatusEvent]) error {
	machineID := req.Msg.GetMachineId()
	if machineID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	if _, ok := s.store.GetMachine(machineID); !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", machineID))
	}

	// Initial full snapshot (FRONTEND.md §2.3 — same mental model as agent resync).
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

func (s *Server) RegisterArtifact(_ context.Context, req *connect.Request[pb.RegisterArtifactRequest]) (*connect.Response[pb.RegisterArtifactResponse], error) {
	if err := s.store.RegisterArtifact(req.Msg.GetArtifact()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&pb.RegisterArtifactResponse{}), nil
}

func (s *Server) ListArtifacts(_ context.Context, req *connect.Request[pb.ListArtifactsRequest]) (*connect.Response[pb.ListArtifactsResponse], error) {
	arts := s.store.ListArtifacts(req.Msg.GetName())
	return connect.NewResponse(&pb.ListArtifactsResponse{Artifacts: arts}), nil
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
