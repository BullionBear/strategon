package store

import (
	"fmt"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"google.golang.org/protobuf/proto"
)

// Catalog ingest lifecycle states. Stored on the artifacts table / in-memory
// record — never on ArtifactRef (which flows into DesiredState).
const (
	ArtifactStateReady   = "READY"
	ArtifactStatePending = "PENDING"
	ArtifactStateFailed  = "FAILED"
)

// ArtifactRecord is one catalog row: the wire ArtifactRef plus ingest state.
type ArtifactRecord struct {
	Ref         *pb.ArtifactRef
	State       string
	StateReason string
}

// CloneArtifactRecord returns a deep copy safe for callers to mutate.
func CloneArtifactRecord(r *ArtifactRecord) *ArtifactRecord {
	if r == nil {
		return nil
	}
	out := &ArtifactRecord{
		State:       r.State,
		StateReason: r.StateReason,
	}
	if r.Ref != nil {
		out.Ref = proto.Clone(r.Ref).(*pb.ArtifactRef)
	}
	return out
}

// NormalizeArtifactState returns a canonical state or an error.
func NormalizeArtifactState(state string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "", ArtifactStateReady:
		return ArtifactStateReady, nil
	case ArtifactStatePending:
		return ArtifactStatePending, nil
	case ArtifactStateFailed:
		return ArtifactStateFailed, nil
	default:
		return "", fmt.Errorf("unknown artifact state %q", state)
	}
}
