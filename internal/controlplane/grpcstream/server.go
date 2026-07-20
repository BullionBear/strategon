// Package grpcstream implements the southbound/northbound AgentService bidi
// stream endpoint. The agent dials in (outbound); the
// control plane never initiates a connection. The main mechanism is pushing
// full DesiredState snapshots: on (re)connect, on every spec change, and on a
// periodic resync to correct silent drift.
package grpcstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/clock"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
)

// Server implements strategyplatformv1connect.AgentServiceHandler.
type Server struct {
	store  store.Store
	clock  clock.Clock
	resync time.Duration
	logger *slog.Logger

	mu        sync.Mutex
	notifiers map[string]chan struct{} // machineID -> re-push signal
}

// Option configures a Server.
type Option func(*Server)

// WithResync sets the periodic full-resync interval.
func WithResync(d time.Duration) Option { return func(s *Server) { s.resync = d } }

// WithClock injects a clock (tests).
func WithClock(c clock.Clock) Option { return func(s *Server) { s.clock = c } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.logger = l } }

// New constructs a Server backed by st.
func New(st store.Store, opts ...Option) *Server {
	s := &Server{
		store:     st,
		clock:     clock.Real{},
		resync:    30 * time.Second,
		logger:    slog.Default(),
		notifiers: map[string]chan struct{}{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Notify signals a connected agent to re-receive its DesiredState snapshot
// (called by the human API after a spec change).
func (s *Server) Notify(machineID string) {
	s.mu.Lock()
	ch := s.notifiers[machineID]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default: // a re-push is already pending
	}
}

// Enroll is the online token→CSR bootstrap. Offline CA issuance
// via strategon-ca covers the current mTLS path; online enrollment remains
// unimplemented so we never mint insecure certs from this RPC.
func (s *Server) Enroll(context.Context, *connect.Request[pb.EnrollRequest]) (*connect.Response[pb.EnrollResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented,
		errors.New("online enrollment (token→CSR) is not implemented; use strategon-ca for offline mTLS certs"))
}

// Connect handles the agent's bidi stream for its whole session.
func (s *Server) Connect(ctx context.Context, stream *connect.BidiStream[pb.AgentMessage, pb.ControlMessage]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first message must be Register"))
	}
	machineID := reg.GetMachineId()
	if peerCN, ok := mtls.PeerCN(ctx); ok && peerCN != machineID {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("Register.machine_id %q does not match client certificate CN %q", machineID, peerCN))
	}
	if _, err := s.store.UpsertMachine(reg); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	s.logger.Info("agent connected", "machine_id", machineID,
		"agent_version", reg.GetAgentVersion(), "agent_build_version", reg.GetAgentBuildVersion())

	notify := s.registerNotifier(machineID)
	defer s.unregisterNotifier(machineID)
	defer func() { _ = s.store.SetReachable(machineID, false) }()

	// Initial full snapshot on connect (idempotent; agent diffs and converges).
	if err := s.pushDesired(stream, machineID); err != nil {
		return err
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			s.handleAgentMessage(machineID, msg)
		}
	}()

	resync := s.clock.Ticker(s.resync)
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-notify:
			if err := s.pushDesired(stream, machineID); err != nil {
				return err
			}
		case <-resync.C():
			if err := s.pushDesired(stream, machineID); err != nil {
				return err
			}
		}
	}
}

func (s *Server) pushDesired(stream *connect.BidiStream[pb.AgentMessage, pb.ControlMessage], machineID string) error {
	ds, ok := s.store.DesiredState(machineID)
	if !ok {
		return nil
	}
	return stream.Send(&pb.ControlMessage{Payload: &pb.ControlMessage_DesiredState{DesiredState: ds}})
}

func (s *Server) handleAgentMessage(machineID string, msg *pb.AgentMessage) {
	switch p := msg.GetPayload().(type) {
	case *pb.AgentMessage_Heartbeat:
		_ = s.store.ApplyHeartbeat(machineID, p.Heartbeat, s.clock.Now().Unix())
	case *pb.AgentMessage_StatusReport:
		_ = s.store.ApplyStatus(machineID, p.StatusReport)
	case *pb.AgentMessage_Event:
		s.logger.Info("agent event", "machine_id", machineID,
			"strategy", p.Event.GetStrategy(), "reason", p.Event.GetReason(),
			"severity", p.Event.GetSeverity().String(), "message", p.Event.GetMessage())
	case *pb.AgentMessage_Nack:
		s.logger.Warn("agent nack", "machine_id", machineID,
			"in_reply_to", p.Nack.GetInReplyTo(), "reason", p.Nack.GetReason())
	case *pb.AgentMessage_LeaseRequest, *pb.AgentMessage_LeaseRenew:
		// Lease lifecycle is owned by the strategy SDK via LeaseService
		// the agent stream does not participate.
		s.logger.Debug("lease stream message ignored (SDK-owned)", "machine_id", machineID)
	default:
		s.logger.Warn("unhandled agent message", "machine_id", machineID)
	}
}

func (s *Server) registerNotifier(machineID string) chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.notifiers[machineID] = ch
	s.mu.Unlock()
	return ch
}

func (s *Server) unregisterNotifier(machineID string) {
	s.mu.Lock()
	delete(s.notifiers, machineID)
	s.mu.Unlock()
}
