// Package assign is the single write path for strategy assignments:
// resolve is the caller's job; this package owns lease interlock, reservation,
// store write, audit, and agent notify.
package assign

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Notifier pushes a fresh DesiredState to a connected agent after a write.
type Notifier interface {
	Notify(machineID string)
}

// Reservation reports whether a cluster already owns a machine+strategy pair.
// E1 ships a no-op; E3 implements it so human verbs cannot clobber owned slots.
type Reservation interface {
	ReservedBy(machineID, strategy string) (cluster string, ok bool)
}

// Request is one assignment write (or undeploy when Spec is nil).
type Request struct {
	MachineID, Strategy    string
	Spec                   *pb.StrategyAssignmentSpec // nil = undeploy
	Action                 string
	Actor                  string
	FromVersion, ToVersion string
	Detail                 string
	// EnforceLeaseInterlock rejects the write when another machine holds an
	// unexpired lease for Strategy. Human Deploy / SetDeployment / Rollback
	// and ApplyAssignment set it; controllers that own a strategy name do not.
	EnforceLeaseInterlock bool
	// AllowReserved writes even when a cluster owns the strategy name.
	// Controllers that own the name set this; human verbs do not.
	AllowReserved bool
}

// Service applies assignment writes through the store.
type Service struct {
	Store       store.Store
	Agents      Notifier
	Reservation Reservation
}

// New constructs an assign service.
func New(st store.Store, agents Notifier) *Service {
	return &Service{Store: st, Agents: agents}
}

// Apply writes the assignment. When the store reports changed=false it skips
// the audit entry and agents.Notify and returns the current generation.
func (s *Service) Apply(_ context.Context, req Request) (gen int64, changed bool, err error) {
	if req.MachineID == "" || req.Strategy == "" {
		return 0, false, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id and strategy are required"))
	}
	if !req.AllowReserved && s.Reservation != nil {
		if cluster, ok := s.Reservation.ReservedBy(req.MachineID, req.Strategy); ok {
			return 0, false, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("strategy %q on machine %q is owned by NatsCluster %q", req.Strategy, req.MachineID, cluster))
		}
	}
	if req.EnforceLeaseInterlock {
		if blocked, reason := store.DeploymentBlockedByLease(s.Store, req.MachineID, req.Strategy); blocked {
			return 0, false, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("migration interlocking: lease for %q %s", req.Strategy, reason))
		}
	}
	gen, changed, err = s.Store.SetAssignment(req.MachineID, req.Strategy, req.Spec)
	if err != nil {
		return 0, false, connect.NewError(connect.CodeInternal, err)
	}
	if !changed {
		return gen, false, nil
	}
	_ = s.Store.AppendAudit(&pb.AuditEntry{
		Timestamp:   timestamppb.Now(),
		Actor:       req.Actor,
		Action:      req.Action,
		MachineId:   req.MachineID,
		Strategy:    req.Strategy,
		FromVersion: req.FromVersion,
		ToVersion:   req.ToVersion,
		Detail:      req.Detail,
	})
	if s.Agents != nil {
		s.Agents.Notify(req.MachineID)
	}
	return gen, true, nil
}
