package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/proto"
)

func (s *Server) ApplyNatsCluster(ctx context.Context, req *connect.Request[pb.ApplyNatsClusterRequest]) (*connect.Response[pb.ApplyNatsClusterResponse], error) {
	in := req.Msg.GetCluster()
	if in == nil || in.GetMetadata().GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster metadata.name is required"))
	}
	if in.GetSpec() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster spec is required"))
	}
	spec, err := s.normalizeAndValidateClusterSpec(in.GetSpec())
	if err != nil {
		return nil, err
	}
	strategy := store.ClusterStrategy(&pb.NatsCluster{Spec: spec})
	if err := s.rejectUnownedStrategy(in.GetMetadata().GetName(), strategy, spec.GetServers()); err != nil {
		return nil, err
	}
	next := &pb.NatsCluster{
		Metadata: in.GetMetadata(),
		Spec:     spec,
	}
	out, _, err := s.store.ApplyNatsCluster(next)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Actor:    auth.ActorFromContext(ctx),
		Action:   "ApplyNatsCluster",
		Strategy: strategy,
		Detail:   out.GetMetadata().GetName(),
	})
	s.logger.Info("apply_nats_cluster", "name", out.GetMetadata().GetName(),
		"generation", out.GetMetadata().GetGeneration(), "actor", auth.ActorFromContext(ctx))
	return connect.NewResponse(&pb.ApplyNatsClusterResponse{Cluster: out}), nil
}

func (s *Server) GetNatsCluster(_ context.Context, req *connect.Request[pb.GetNatsClusterRequest]) (*connect.Response[pb.NatsCluster], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	c, ok := s.store.GetNatsCluster(name)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("nats cluster %q not found", name))
	}
	return connect.NewResponse(c), nil
}

func (s *Server) ListNatsClusters(_ context.Context, _ *connect.Request[pb.ListNatsClustersRequest]) (*connect.Response[pb.ListNatsClustersResponse], error) {
	return connect.NewResponse(&pb.ListNatsClustersResponse{Clusters: s.store.ListNatsClusters()}), nil
}

func (s *Server) DeleteNatsCluster(ctx context.Context, req *connect.Request[pb.DeleteNatsClusterRequest]) (*connect.Response[pb.DeleteNatsClusterResponse], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if _, ok := s.store.GetNatsCluster(name); !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("nats cluster %q not found", name))
	}
	if _, err := s.store.MarkNatsClusterDeleting(name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Actor:  auth.ActorFromContext(ctx),
		Action: "DeleteNatsCluster",
		Detail: name,
	})
	return connect.NewResponse(&pb.DeleteNatsClusterResponse{}), nil
}

func (s *Server) normalizeAndValidateClusterSpec(in *pb.NatsClusterSpec) (*pb.NatsClusterSpec, error) {
	spec := proto.Clone(in).(*pb.NatsClusterSpec)
	if spec.Strategy == "" {
		spec.Strategy = "nats"
	}
	if spec.GetArtifactVersion() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("artifact_version is required"))
	}
	if len(spec.GetServers()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("servers must not be empty"))
	}
	if spec.Update == nil {
		spec.Update = &pb.NatsClusterUpdate{}
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
	for i, srv := range spec.GetServers() {
		if srv.GetMachine() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("servers[%d]: machine is required", i))
		}
		if srv.GetServerName() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("servers[%d]: server_name is required", i))
		}
		if srv.GetRouteHost() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("servers[%d]: route_host is required", i))
		}
		if _, ok := s.store.GetMachine(srv.GetMachine()); !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("servers[%d]: machine %q not registered", i, srv.GetMachine()))
		}
		if _, dup := seenName[srv.GetServerName()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate server_name %q", srv.GetServerName()))
		}
		if _, dup := seenMachine[srv.GetMachine()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate machine %q", srv.GetMachine()))
		}
		seenName[srv.GetServerName()] = struct{}{}
		seenMachine[srv.GetMachine()] = struct{}{}
		if srv.ClientPort <= 0 {
			srv.ClientPort = 4222
		}
		if srv.ClusterPort <= 0 {
			srv.ClusterPort = 6222
		}
		if srv.MonitorPort <= 0 {
			srv.MonitorPort = 8222
		}
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
		for _, srv := range spec.GetServers() {
			rec, _ := s.store.GetMachine(srv.GetMachine())
			if err := requireOCISupported(rec); err != nil {
				return nil, err
			}
		}
	}
	return spec, nil
}

func (s *Server) rejectUnownedStrategy(clusterName, strategy string, servers []*pb.NatsServer) error {
	for _, srv := range servers {
		owner, reserved := s.store.ReservedBy(srv.GetMachine(), strategy)
		if reserved && owner != clusterName {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("machine %q strategy %q is owned by NatsCluster %q", srv.GetMachine(), strategy, owner))
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
