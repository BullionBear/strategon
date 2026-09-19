package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/secrets"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// WithSecrets attaches SecretManagement. nil (or a dark module) leaves
// catalog RPCs FailedPrecondition and apply of secret. refs rejected.
func (s *Server) WithSecrets(m *secrets.Module) *Server {
	s.secrets = m
	return s
}

func (s *Server) PutSecret(ctx context.Context, req *connect.Request[pb.PutSecretRequest]) (*connect.Response[pb.PutSecretResponse], error) {
	if s.secrets.Dark() {
		return nil, secretError(secrets.ErrDark)
	}
	name := strings.TrimSpace(req.Msg.GetName())
	token, view, err := s.secrets.Put(ctx, name, req.Msg.GetValue())
	if err != nil {
		return nil, secretError(err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp: timestamppb.Now(),
		Actor:     auth.ActorFromContext(ctx),
		Action:    "PutSecret",
		Detail:    fmt.Sprintf("name=%s bytes=%d key_id=%s", name, view.LengthBytes, view.KeyID),
	})
	for _, id := range machinesReferencingSecret(s.store, token) {
		if s.agents != nil {
			s.agents.Notify(id)
		}
	}
	return connect.NewResponse(&pb.PutSecretResponse{Token: token}), nil
}

func (s *Server) GetSecret(ctx context.Context, req *connect.Request[pb.GetSecretRequest]) (*connect.Response[pb.GetSecretResponse], error) {
	view, err := s.secrets.Get(ctx, strings.TrimSpace(req.Msg.GetName()))
	if err != nil {
		return nil, secretError(err)
	}
	return connect.NewResponse(&pb.GetSecretResponse{Secret: secretView(view)}), nil
}

func (s *Server) ListSecrets(ctx context.Context, _ *connect.Request[pb.ListSecretsRequest]) (*connect.Response[pb.ListSecretsResponse], error) {
	views, err := s.secrets.List(ctx)
	if err != nil {
		return nil, secretError(err)
	}
	out := make([]*pb.SecretView, 0, len(views))
	for _, v := range views {
		out = append(out, secretView(v))
	}
	return connect.NewResponse(&pb.ListSecretsResponse{Secrets: out}), nil
}

func (s *Server) DeleteSecret(ctx context.Context, req *connect.Request[pb.DeleteSecretRequest]) (*connect.Response[pb.DeleteSecretResponse], error) {
	if s.secrets.Dark() {
		return nil, secretError(secrets.ErrDark)
	}
	name := strings.TrimSpace(req.Msg.GetName())
	token := secrets.Token(name)
	if err := s.secrets.Delete(ctx, name); err != nil {
		return nil, secretError(err)
	}
	_ = s.store.AppendAudit(&pb.AuditEntry{
		Timestamp: timestamppb.Now(),
		Actor:     auth.ActorFromContext(ctx),
		Action:    "DeleteSecret",
		Detail:    fmt.Sprintf("name=%s", name),
	})
	for _, id := range machinesReferencingSecret(s.store, token) {
		if s.agents != nil {
			s.agents.Notify(id)
		}
	}
	return connect.NewResponse(&pb.DeleteSecretResponse{}), nil
}

func secretView(v secrets.View) *pb.SecretView {
	return &pb.SecretView{
		Name:        v.Name,
		Token:       v.Token,
		LengthBytes: v.LengthBytes,
		KeyId:       v.KeyID,
	}
}

func secretError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, secrets.ErrDark):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, secrets.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, secrets.ErrTooLarge), errors.Is(err, secrets.ErrBadRef), errors.Is(err, secrets.ErrOpen):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
}

func (s *Server) rejectSecretRefs(ctx context.Context, maps ...map[string]string) error {
	if err := secrets.ValidateMaps(ctx, s.secrets, maps...); err != nil {
		return secretError(err)
	}
	return nil
}

func machinesReferencingSecret(st store.Store, token string) []string {
	if st == nil || token == "" {
		return nil
	}
	var out []string
	for _, rec := range st.ListMachines() {
		hit := false
		for _, spec := range rec.Assignments {
			for _, v := range spec.GetEnv() {
				if v == token {
					out = append(out, rec.MachineID)
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
	}
	return out
}
