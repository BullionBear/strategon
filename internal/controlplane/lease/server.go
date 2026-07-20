// Package lease implements LeaseService: unary fencing-lease RPCs for strategy
// SDKs. Served on the agent port alongside AgentService.
package lease

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements strategyplatformv1connect.LeaseServiceHandler.
type Server struct {
	store  store.Store
	logger *slog.Logger
}

// New constructs a LeaseService handler.
func New(st store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: st, logger: logger}
}

func (s *Server) Acquire(ctx context.Context, req *connect.Request[pb.LeaseRequest]) (*connect.Response[pb.LeaseResponse], error) {
	msg := req.Msg
	machineID := msg.GetMachineId()
	if machineID == "" || msg.GetStrategy() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("machine_id and strategy are required"))
	}
	if err := s.authorizeMachine(ctx, machineID); err != nil {
		return nil, err
	}
	ttl := time.Duration(msg.GetRequestedTtlSeconds()) * time.Second
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	res, err := s.store.AcquireLease(machineID, msg.GetStrategy(), ttl)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	out := &pb.LeaseResponse{
		RequestId:  msg.GetRequestId(),
		Granted:    res.Granted,
		LeaseId:    res.LeaseID,
		DenyReason: res.DenyReason,
	}
	if res.Granted {
		out.ExpiresAt = timestamppb.New(res.ExpiresAt)
		s.logger.Info("lease acquired", "strategy", msg.GetStrategy(), "machine_id", machineID, "lease_id", res.LeaseID)
	}
	return connect.NewResponse(out), nil
}

func (s *Server) Renew(ctx context.Context, req *connect.Request[pb.LeaseRenew]) (*connect.Response[pb.LeaseResponse], error) {
	msg := req.Msg
	if msg.GetLeaseId() == "" || msg.GetStrategy() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("lease_id and strategy are required"))
	}
	info, ok := s.store.GetLease(msg.GetStrategy())
	if !ok {
		return connect.NewResponse(&pb.LeaseResponse{
			Granted:    false,
			DenyReason: "no lease",
		}), nil
	}
	if err := s.authorizeMachine(ctx, info.MachineID); err != nil {
		return nil, err
	}
	// ttl<=0 → store reuses the TTL from Acquire.
	res, err := s.store.RenewLease(info.MachineID, msg.GetStrategy(), msg.GetLeaseId(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	out := &pb.LeaseResponse{
		Granted:    res.Granted,
		LeaseId:    res.LeaseID,
		DenyReason: res.DenyReason,
	}
	if res.Granted {
		out.ExpiresAt = timestamppb.New(res.ExpiresAt)
	}
	return connect.NewResponse(out), nil
}

func (s *Server) authorizeMachine(ctx context.Context, machineID string) error {
	if cn, ok := mtls.PeerCN(ctx); ok && cn != machineID {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("client certificate CN %q does not match machine_id %q", cn, machineID))
	}
	return nil
}
