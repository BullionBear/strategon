package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/filebrowse"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/filetransfer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) ListStrategySlots(_ context.Context, req *connect.Request[pb.ListStrategySlotsRequest]) (*connect.Response[pb.ListStrategySlotsResponse], error) {
	id := req.Msg.GetMachineId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	rec, ok := s.store.GetMachine(id)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", id))
	}
	var slots []*pb.StrategySlotStatus
	if rec.SlotsStatus != nil {
		slots = rec.SlotsStatus.GetSlots()
	}
	sort.Slice(slots, func(i, j int) bool {
		return slots[i].GetStrategy() < slots[j].GetStrategy()
	})
	out := make([]*pb.StrategySlotView, 0, len(slots))
	var total int64
	for _, st := range slots {
		if st == nil || st.GetStrategy() == "" {
			continue
		}
		view := &pb.StrategySlotView{
			Strategy:       st.GetStrategy(),
			SizeBytes:      st.GetSizeBytes(),
			CurrentVersion: st.GetCurrentVersion(),
			Assigned:       rec.Assignments[st.GetStrategy()] != nil,
			SizeComputedAt: st.GetSizeComputedAt(),
		}
		total += st.GetSizeBytes()
		out = append(out, view)
	}
	return connect.NewResponse(&pb.ListStrategySlotsResponse{Slots: out, TotalBytes: total}), nil
}

func (s *Server) ReapStrategies(ctx context.Context, req *connect.Request[pb.ReapStrategiesRequest]) (*connect.Response[pb.ReapStrategiesResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	names := uniqueNonEmpty(msg.GetStrategies())
	if len(names) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("strategies is required"))
	}
	for _, name := range names {
		if err := filebrowse.ValidateStrategy(name); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("strategy %q: %w", name, err))
		}
	}
	rec, ok := s.store.GetMachine(msg.GetMachineId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	if !rec.Reachable {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %q is not reachable", msg.GetMachineId()))
	}
	if err := requireAgentCapability(msg.GetMachineId(), rec, MinReapStrategiesAgentVersion, "reap strategies"); err != nil {
		return nil, err
	}
	ctrl, err := s.agentControl()
	if err != nil {
		return nil, err
	}
	if s.broker == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("file transfer broker not configured"))
	}

	reqID, reapCh, cancel := s.broker.NewReap(msg.GetMachineId())
	defer cancel()

	if err := ctrl.SendControl(ctx, msg.GetMachineId(), &pb.ControlMessage{
		MessageId: reqID,
		Payload: &pb.ControlMessage_ReapStrategies{ReapStrategies: &pb.ReapStrategies{
			RequestId:  reqID,
			Strategies: names,
		}},
	}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(filetransfer.ReapTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("reap timed out waiting for agent"))
	case result := <-reapCh:
		if result.GetError() != "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(result.GetError()))
		}
		_ = s.store.AppendAudit(&pb.AuditEntry{
			Timestamp: timestamppb.Now(),
			Actor:     auth.ActorFromContext(ctx),
			Action:    "ReapStrategies",
			MachineId: msg.GetMachineId(),
			Detail:    reapAuditDetail(result.GetResults()),
		})
		return connect.NewResponse(&pb.ReapStrategiesResponse{Results: result.GetResults()}), nil
	}
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func reapAuditDetail(results []*pb.ReapStrategyResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteByte('\n')
		}
		if r.GetRemoved() {
			fmt.Fprintf(&b, "%s removed freed=%d", r.GetStrategy(), r.GetFreedBytes())
			continue
		}
		fmt.Fprintf(&b, "%s error=%s", r.GetStrategy(), r.GetError())
	}
	return b.String()
}
