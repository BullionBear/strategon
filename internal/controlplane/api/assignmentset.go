package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/assignmentset"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/proto"
)

func (s *Server) ApplyAssignmentSet(ctx context.Context, req *connect.Request[pb.ApplyAssignmentSetRequest]) (*connect.Response[pb.ApplyAssignmentSetResponse], error) {
	in := req.Msg.GetSet()
	if in == nil || in.GetMetadata().GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster metadata.name is required"))
	}
	if in.GetSpec() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster spec is required"))
	}
	spec, err := s.normalizeAndValidateSetSpec(in.GetMetadata().GetName(), in.GetSpec())
	if err != nil {
		return nil, err
	}
	strategy := store.SetStrategy(&pb.AssignmentSet{Spec: spec})
	if err := s.rejectUnownedStrategy(in.GetMetadata().GetName(), strategy, spec.GetMembers()); err != nil {
		return nil, err
	}
	next := &pb.AssignmentSet{
		Metadata: in.GetMetadata(),
		Spec:     spec,
	}
	out, _, err := s.store.ApplyAssignmentSet(next)
	if err != nil {
		// The store re-checks ownership under its write lock, so a race that
		// slipped past rejectUnownedStrategy surfaces here as the same class of
		// error the admission check would have returned.
		var conflict *store.ReservationConflictError
		if errors.As(err, &conflict) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Actor:    auth.ActorFromContext(ctx),
		Action:   "ApplyAssignmentSet",
		Strategy: strategy,
		Detail:   out.GetMetadata().GetName(),
	})
	s.logger.Info("apply_nats_cluster", "name", out.GetMetadata().GetName(),
		"generation", out.GetMetadata().GetGeneration(), "actor", auth.ActorFromContext(ctx))
	return connect.NewResponse(&pb.ApplyAssignmentSetResponse{Set: out}), nil
}

func (s *Server) GetAssignmentSet(_ context.Context, req *connect.Request[pb.GetAssignmentSetRequest]) (*connect.Response[pb.AssignmentSet], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	c, ok := s.store.GetAssignmentSet(name)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("nats cluster %q not found", name))
	}
	return connect.NewResponse(c), nil
}

func (s *Server) ListAssignmentSets(_ context.Context, _ *connect.Request[pb.ListAssignmentSetsRequest]) (*connect.Response[pb.ListAssignmentSetsResponse], error) {
	return connect.NewResponse(&pb.ListAssignmentSetsResponse{Sets: s.store.ListAssignmentSets()}), nil
}

func (s *Server) DeleteAssignmentSet(ctx context.Context, req *connect.Request[pb.DeleteAssignmentSetRequest]) (*connect.Response[pb.DeleteAssignmentSetResponse], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if _, ok := s.store.GetAssignmentSet(name); !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("nats cluster %q not found", name))
	}
	if _, err := s.store.MarkAssignmentSetDeleting(name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Actor:  auth.ActorFromContext(ctx),
		Action: "DeleteAssignmentSet",
		Detail: name,
	})
	return connect.NewResponse(&pb.DeleteAssignmentSetResponse{}), nil
}

func (s *Server) normalizeAndValidateSetSpec(name string, in *pb.AssignmentSetSpec) (*pb.AssignmentSetSpec, error) {
	spec := proto.Clone(in).(*pb.AssignmentSetSpec)
	if spec.Strategy == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("spec.strategy is required"))
	}
	if spec.GetArtifactVersion() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("artifact_version is required"))
	}
	if len(spec.GetMembers()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("members must not be empty"))
	}
	if spec.Update == nil {
		spec.Update = &pb.RollingUpdate{}
	}
	if spec.Update.MaxUnavailable < 1 {
		if spec.Update.MaxUnavailable == 0 {
			spec.Update.MaxUnavailable = 1
		} else {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("update.max_unavailable must be >= 1"))
		}
	}
	if spec.Update.WaitReadySeconds <= 0 {
		spec.Update.WaitReadySeconds = 60
	}

	seenName := map[string]struct{}{}
	seenMachine := map[string]struct{}{}
	for i, srv := range spec.GetMembers() {
		if srv.GetMachine() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: machine is required", i))
		}
		if srv.GetName() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: name is required", i))
		}
		if _, ok := s.store.GetMachine(srv.GetMachine()); !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: machine %q not registered", i, srv.GetMachine()))
		}
		if _, dup := seenName[srv.GetName()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate member name %q", srv.GetName()))
		}
		if _, dup := seenMachine[srv.GetMachine()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate machine %q", srv.GetMachine()))
		}
		seenName[srv.GetName()] = struct{}{}
		seenMachine[srv.GetMachine()] = struct{}{}
	}

	// Render every member now so an unknown ${...} is rejected here rather than
	// failing on the machine when the agent cannot expand it.
	if err := assignmentset.Validate(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: name},
		Spec:     spec,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	art, err := s.resolveArtifact(spec.Strategy, spec.Strategy, spec.GetArtifactVersion())
	if err != nil {
		return nil, err
	}
	if err := s.requireArtifactReady(art.GetName(), art.GetVersion()); err != nil {
		return nil, err
	}
	if spec.GetConfigVersion() != "" {
		cfg, err := s.resolveArtifact(art.GetName()+"-config", spec.Strategy+"-config", spec.GetConfigVersion())
		if err != nil {
			return nil, err
		}
		if err := s.requireArtifactReady(cfg.GetName(), cfg.GetVersion()); err != nil {
			return nil, err
		}
	}
	if art.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE {
		for _, srv := range spec.GetMembers() {
			rec, _ := s.store.GetMachine(srv.GetMachine())
			if err := requireOCISupported(rec); err != nil {
				return nil, err
			}
		}
	}
	return spec, nil
}

func (s *Server) rejectUnownedStrategy(clusterName, strategy string, servers []*pb.SetMember) error {
	for _, srv := range servers {
		owner, reserved := s.store.ReservedBy(srv.GetMachine(), strategy)
		if reserved && owner != clusterName {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("machine %q strategy %q is owned by AssignmentSet %q", srv.GetMachine(), strategy, owner))
		}
		rec, ok := s.store.GetMachine(srv.GetMachine())
		if !ok || rec.Assignments[strategy] == nil {
			continue
		}
		if !reserved {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("machine %q already has an unowned %q assignment", srv.GetMachine(), strategy))
		}
	}
	return nil
}
