package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	if err := s.rejectUnownedSlots(in.GetMetadata().GetName(), spec); err != nil {
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

	if spec.GetConfigVersion() == "latest" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("config_version must not be \"latest\""))
	}

	seenName := map[string]struct{}{}
	for i, srv := range spec.GetMembers() {
		if srv.GetMachine() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: machine is required", i))
		}
		srv.Name = strings.TrimSpace(srv.GetName())
		if srv.GetName() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: name is required", i))
		}
		if err := assignmentset.ValidateMemberName(srv.GetName()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: %w", i, err))
		}
		if srv.GetConfigVersion() == "latest" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: config_version must not be \"latest\"", i))
		}
		rec, ok := s.store.GetMachine(srv.GetMachine())
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("members[%d]: machine %q not registered", i, srv.GetMachine()))
		}
		if spec.GetTemplate().GetCaptureStdio() {
			if err := requireAgentCapability(srv.GetMachine(), rec, MinStdioCaptureAgentVersion, "stdio capture"); err != nil {
				return nil, err
			}
		}
		if _, dup := seenName[srv.GetName()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate member name %q", srv.GetName()))
		}
		seenName[srv.GetName()] = struct{}{}
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
	set := &pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: name},
		Spec:     spec,
	}
	for i := range spec.GetMembers() {
		want, err := assignmentset.WantConfig(set, i, art.GetName())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if want.Version == "" {
			continue
		}
		cfg, err := s.resolveArtifact(want.Primary, want.Fallback, want.Version)
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

func (s *Server) rejectUnownedSlots(clusterName string, spec *pb.AssignmentSetSpec) error {
	existing, ok := s.store.GetAssignmentSet(clusterName)
	checkFamily := !ok || existing.GetStatus().GetAssignmentKey() == ""

	type slot struct{ machine, strategy string }
	var checks []slot
	seen := map[slot]struct{}{}
	add := func(machine, strategy string) {
		k := slot{machine, strategy}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		checks = append(checks, k)
	}
	for _, srv := range spec.GetMembers() {
		add(srv.GetMachine(), srv.GetName())
	}
	if checkFamily {
		cat := spec.GetStrategy()
		for _, srv := range spec.GetMembers() {
			if srv.GetName() != cat {
				add(srv.GetMachine(), cat)
			}
		}
	}

	reservedOn := map[string]map[string]struct{}{}
	for _, ch := range checks {
		if reservedOn[ch.machine] == nil {
			m := map[string]struct{}{}
			for _, name := range s.store.ReservedSlots(ch.machine) {
				m[name] = struct{}{}
			}
			reservedOn[ch.machine] = m
		}
		if _, reserved := reservedOn[ch.machine][ch.strategy]; reserved {
			owner, _ := s.store.ReservedBy(ch.machine, ch.strategy)
			if owner != clusterName {
				return connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("machine %q strategy %q is owned by AssignmentSet %q", ch.machine, ch.strategy, owner))
			}
			continue
		}
		rec, ok := s.store.GetMachine(ch.machine)
		if ok && rec.Assignments[ch.strategy] != nil {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("machine %q already has an unowned %q assignment", ch.machine, ch.strategy))
		}
	}
	return nil
}
