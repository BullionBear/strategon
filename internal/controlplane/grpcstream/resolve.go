package grpcstream

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
	"github.com/bullionbear/strategon/internal/controlplane/objectstore"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ResolveArtifactSource exchanges an s3:// catalog URI for a short-lived
// presigned HTTPS URL. The caller's machine identity is the mTLS peer CN;
// the artifact must appear in that machine's desired state (assignment
// artifact/config or shared file), otherwise PermissionDenied + audit.
func (s *Server) ResolveArtifactSource(ctx context.Context, req *connect.Request[pb.ResolveArtifactSourceRequest]) (*connect.Response[pb.ResolveArtifactSourceResponse], error) {
	name := req.Msg.GetName()
	version := req.Msg.GetVersion()
	if name == "" || version == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name and version are required"))
	}

	machineID, ok := mtls.PeerCN(ctx)
	if !ok {
		s.auditResolveDenied("", name, version, "missing mTLS client certificate CN")
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("mTLS client certificate required"))
	}

	rec, found := s.store.GetMachine(machineID)
	if !found {
		s.auditResolveDenied(machineID, name, version, "unknown machine")
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("machine %q is not enrolled", machineID))
	}
	if !machineReferencesArtifact(rec, name, version) {
		s.auditResolveDenied(machineID, name, version, "artifact not in desired state")
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("machine %q is not assigned artifact %s@%s", machineID, name, version))
	}

	ref, ok := s.store.GetArtifact(name, version)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("artifact %s@%s not in catalog", name, version))
	}
	if !artifacturi.IsS3(ref.GetUri()) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("artifact %s@%s uri is not s3:// (got %q)", name, version, ref.GetUri()))
	}
	obj, err := artifacturi.ParseS3(ref.GetUri())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if s.objects == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("object store is not configured"))
	}

	url, expiresAt, err := s.objects.PresignGet(ctx, obj.Bucket, obj.Key, objectstore.DefaultPresignTTL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.ResolveArtifactSourceResponse{
		Url:       url,
		ExpiresAt: timestamppb.New(expiresAt),
	}), nil
}

// machineReferencesArtifact reports whether the machine's desired state
// references name+version via an assignment artifact/config or a shared file.
func machineReferencesArtifact(rec *store.MachineRecord, name, version string) bool {
	if rec == nil {
		return false
	}
	for _, a := range rec.Assignments {
		if artifactMatch(a.GetArtifact(), name, version) || artifactMatch(a.GetConfig(), name, version) {
			return true
		}
	}
	for _, f := range rec.SharedFiles {
		if artifactMatch(f.GetArtifact(), name, version) {
			return true
		}
	}
	return false
}

func artifactMatch(ref *pb.ArtifactRef, name, version string) bool {
	return ref != nil && ref.GetName() == name && ref.GetVersion() == version
}

func (s *Server) auditResolveDenied(machineID, name, version, reason string) {
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp: timestamppb.Now(),
		Actor:     machineID,
		Action:    "ResolveArtifactSource",
		MachineId: machineID,
		Strategy:  name,
		ToVersion: version,
		Detail:    "denied: " + reason,
	})
}
