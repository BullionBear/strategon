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
		machines = append(machines, BuildMachine(rec))
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
	return connect.NewResponse(BuildMachine(rec)), nil
}

func (s *Server) Deploy(_ context.Context, req *connect.Request[pb.DeployRequest]) (*connect.Response[pb.DeployResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" || msg.GetStrategy() == "" || msg.GetArtifactVersion() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id, strategy, and artifact_version are required"))
	}
	rec, ok := s.store.GetMachine(msg.GetMachineId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}

	artName := msg.GetStrategy()
	if existing := rec.Assignments[msg.GetStrategy()]; existing != nil && existing.GetArtifact().GetName() != "" {
		artName = existing.GetArtifact().GetName()
	}
	art, ok := s.store.GetArtifact(artName, msg.GetArtifactVersion())
	if !ok {
		// Fall back: try strategy name as artifact name (common convention).
		art, ok = s.store.GetArtifact(msg.GetStrategy(), msg.GetArtifactVersion())
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("artifact %q version %q not registered; call RegisterArtifact first", artName, msg.GetArtifactVersion()))
	}

	spec := defaultOrCloneSpec(rec.Assignments[msg.GetStrategy()], msg.GetStrategy())
	fromVersion := spec.GetArtifact().GetVersion()
	spec.Artifact = art

	if cv := msg.GetConfigVersion(); cv != "" {
		cfg, ok := s.store.GetArtifact(art.GetName()+"-config", cv)
		if !ok {
			cfg, ok = s.store.GetArtifact(msg.GetStrategy()+"-config", cv)
		}
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("config version %q not registered", cv))
		}
		spec.Config = cfg
	}

	gen, err := s.store.SetAssignment(msg.GetMachineId(), msg.GetStrategy(), spec)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       "local",
		Action:      "Deploy",
		MachineId:   msg.GetMachineId(),
		Strategy:    msg.GetStrategy(),
		FromVersion: fromVersion,
		ToVersion:   art.GetVersion(),
	})
	if s.agents != nil {
		s.agents.Notify(msg.GetMachineId())
	}
	s.logger.Info("deploy", "machine_id", msg.GetMachineId(), "strategy", msg.GetStrategy(),
		"version", art.GetVersion(), "generation", gen)
	return connect.NewResponse(&pb.DeployResponse{Generation: gen}), nil
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

func (s *Server) SetSchedule(_ context.Context, req *connect.Request[pb.SetScheduleRequest]) (*connect.Response[pb.SetScheduleResponse], error) {
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
		Machine:   BuildMachine(rec),
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
			Startsecs:            2,
			HealthWindowSeconds:  30,
			MaxCrashesInWindow:   3,
			StopGraceSeconds:     10,
			EnableAutoRollback:   true,
		},
	}
}
