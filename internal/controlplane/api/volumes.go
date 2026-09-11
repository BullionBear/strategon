package api

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/assignmentset"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/volume"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) CreateVolume(ctx context.Context, req *connect.Request[pb.CreateVolumeRequest]) (*connect.Response[pb.CreateVolumeResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	if err := volume.ValidateName(msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if _, ok := s.store.GetMachine(msg.GetMachineId()); !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	gen, _, changed, err := s.store.CreateVolume(msg.GetMachineId(), msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if changed {
		_ = s.store.AppendAudit(&pb.AuditEntry{
			Timestamp: timestamppb.Now(),
			Actor:     auth.ActorFromContext(ctx),
			Action:    "CreateVolume",
			MachineId: msg.GetMachineId(),
			Detail:    msg.GetName(),
		})
		if s.agents != nil {
			s.agents.Notify(msg.GetMachineId())
		}
	}
	return connect.NewResponse(&pb.CreateVolumeResponse{Generation: gen}), nil
}

func (s *Server) DeleteVolume(ctx context.Context, req *connect.Request[pb.DeleteVolumeRequest]) (*connect.Response[pb.DeleteVolumeResponse], error) {
	msg := req.Msg
	if msg.GetMachineId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	if err := volume.ValidateName(msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if _, ok := s.store.GetMachine(msg.GetMachineId()); !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", msg.GetMachineId()))
	}
	if reason, occupied := s.volumeOccupied(msg.GetMachineId(), msg.GetName()); occupied {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(reason))
	}
	gen, _, changed, err := s.store.DeleteVolume(msg.GetMachineId(), msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if changed {
		_ = s.store.AppendAudit(&pb.AuditEntry{
			Timestamp: timestamppb.Now(),
			Actor:     auth.ActorFromContext(ctx),
			Action:    "DeleteVolume",
			MachineId: msg.GetMachineId(),
			Detail:    msg.GetName(),
		})
		if s.agents != nil {
			s.agents.Notify(msg.GetMachineId())
		}
	}
	return connect.NewResponse(&pb.DeleteVolumeResponse{Generation: gen}), nil
}

func (s *Server) ListVolumes(_ context.Context, req *connect.Request[pb.ListVolumesRequest]) (*connect.Response[pb.ListVolumesResponse], error) {
	id := req.Msg.GetMachineId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_id is required"))
	}
	rec, ok := s.store.GetMachine(id)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("machine %q not found", id))
	}
	statusByName := map[string]*pb.VolumeStatus{}
	if rec.VolumesStatus != nil {
		for _, v := range rec.VolumesStatus.GetVolumes() {
			statusByName[v.GetName()] = v
		}
	}
	pinned := assignmentMountIndex(rec)
	names := make([]string, 0, len(rec.Volumes)+len(statusByName))
	seen := map[string]struct{}{}
	for n := range rec.Volumes {
		names = append(names, n)
		seen[n] = struct{}{}
	}
	for n := range statusByName {
		if _, ok := seen[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]*pb.VolumeView, 0, len(names))
	for _, n := range names {
		view := &pb.VolumeView{Name: n, PinnedBy: pinned[n]}
		if st := statusByName[n]; st != nil {
			view.Ready = st.GetReady()
			view.SizeBytes = st.GetSizeBytes()
			view.LastError = st.GetLastError()
			view.MountedBy = st.GetMountedBy()
		}
		out = append(out, view)
	}
	return connect.NewResponse(&pb.ListVolumesResponse{Volumes: out}), nil
}

func assignmentMountIndex(rec *store.MachineRecord) map[string][]string {
	out := map[string][]string{}
	if rec == nil {
		return out
	}
	for strat, spec := range rec.Assignments {
		for _, m := range spec.GetVolumeMounts() {
			out[m.GetName()] = append(out[m.GetName()], strat)
		}
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

func (s *Server) volumeOccupied(machineID, name string) (string, bool) {
	rec, ok := s.store.GetMachine(machineID)
	if !ok {
		return "", false
	}
	for strat, spec := range rec.Assignments {
		for _, m := range spec.GetVolumeMounts() {
			if m.GetName() == name {
				return fmt.Sprintf("volume %q is mounted by assignment %q", name, strat), true
			}
		}
	}
	for _, set := range s.store.ListAssignmentSets() {
		members := set.GetSpec().GetMembers()
		for i, mem := range members {
			if mem.GetMachine() != machineID {
				continue
			}
			expanded, err := assignmentset.Expand(set, i)
			if err != nil {
				s.logger.Warn("delete volume: treating AssignmentSet as occupied after expand error",
					"set", set.GetMetadata().GetName(), "member", mem.GetName(), "err", err)
				return fmt.Sprintf("volume %q may be referenced by AssignmentSet %q (expand failed)", name, set.GetMetadata().GetName()), true
			}
			for _, m := range expanded.VolumeMounts {
				if m.GetName() == name {
					return fmt.Sprintf("volume %q is referenced by AssignmentSet %q member %q", name, set.GetMetadata().GetName(), mem.GetName()), true
				}
			}
		}
	}
	return "", false
}

func validateAssignmentMounts(machineID string, rec *store.MachineRecord, mounts []*pb.VolumeMount) error {
	var have map[string]*pb.VolumeSpec
	if rec != nil {
		have = rec.Volumes
	}
	return volume.EnsureInventory(machineID, have, mounts)
}
