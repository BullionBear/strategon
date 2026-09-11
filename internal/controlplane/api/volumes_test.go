package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestCreateListDeleteVolume(t *testing.T) {
	client, st, _, agents := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 3})

	n := agents.n
	resp, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: "m1", Name: "mftik-data",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGeneration() < 1 {
		t.Fatal("expected generation bump")
	}
	if agents.n != n+1 {
		t.Fatalf("notify=%d", agents.n)
	}
	list, err := client.ListVolumes(ctx, connect.NewRequest(&pb.ListVolumesRequest{MachineId: "m1"}))
	if err != nil || len(list.Msg.GetVolumes()) != 1 || list.Msg.GetVolumes()[0].GetName() != "mftik-data" {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if _, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: "m1", Name: "../escape",
	})); err == nil {
		t.Fatal("expected invalid name")
	}
}

func TestDeleteVolumeOccupiedByAssignmentAndSet(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 3})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	if _, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: "m1", Name: "data",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId:       "m1",
		Strategy:        "hello",
		ArtifactVersion: "v1",
		Stopped:         true,
		VolumeMounts:    []*pb.VolumeMount{{Name: "data", ContainerPath: "/var/lib/mftik"}},
	})); err != nil {
		t.Fatal(err)
	}
	_, err := client.DeleteVolume(ctx, connect.NewRequest(&pb.DeleteVolumeRequest{
		MachineId: "m1", Name: "data",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stopped assignment must pin delete: %v", err)
	}

	if _, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: "m1", Name: "set-data",
	})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "bad"},
		Spec: &pb.AssignmentSetSpec{
			Strategy:        "hello",
			ArtifactVersion: "v1",
			Template: &pb.MemberTemplate{
				VolumeMounts: []*pb.VolumeMount{{Name: "${nope}", ContainerPath: "/data"}},
			},
			Members: []*pb.SetMember{{Machine: "m1", Name: "x"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = client.DeleteVolume(ctx, connect.NewRequest(&pb.DeleteVolumeRequest{
		MachineId: "m1", Name: "set-data",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expand error must fail closed: %v", err)
	}
	if !strings.Contains(err.Error(), "expand failed") {
		t.Fatalf("want expand-failed occupancy, got %v", err)
	}
}

func TestVolumeBrowseRequiresAgentVersion3(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 2})
	_, err := client.BrowseDir(ctx, connect.NewRequest(&pb.BrowseDirRequest{
		MachineId: "m1", Volume: "data", Path: ".",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want agent_version>=3, got %v", err)
	}
}

func TestApplyAssignmentRejectsMissingVolume(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	_, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId:       "m1",
		Strategy:        "hello",
		ArtifactVersion: "v1",
		VolumeMounts:    []*pb.VolumeMount{{Name: "missing", ContainerPath: "/data"}},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing volume: %v", err)
	}
}
