package auth

import (
	"context"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

// TokenPersistence is the narrow store surface auth needs for durable tokens.
// controlplane/store.Store satisfies this structurally; tests can fake just
// these methods instead of embedding the full Store.
type TokenPersistence interface {
	LoadAPITokens(ctx context.Context) ([]store.TokenRow, error)
	InsertAPIToken(ctx context.Context, t store.TokenRow) error
	RevokeAPIToken(ctx context.Context, userID, id string) (bool, error)
	TouchAPITokens(ctx context.Context, lastUsed map[string]time.Time) error
	AppendAudit(entry *pb.AuditEntry) error
}
