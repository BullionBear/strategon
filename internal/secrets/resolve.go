package secrets

import (
	"context"
	"fmt"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// ResolveDesiredState unwraps secret.<name> in assignment env on ds in place.
// ds must already be a clone. No refs is a no-op even when the module is dark.
// Any ref failure returns an error; the caller must not send ds.
func ResolveDesiredState(ctx context.Context, m *Module, ds *pb.DesiredState) error {
	if ds == nil {
		return nil
	}
	for _, spec := range ds.GetAssignments() {
		if spec == nil || !MapHasRefs(spec.GetEnv()) {
			continue
		}
		resolved, err := m.ResolveMap(ctx, spec.GetEnv())
		if err != nil {
			return fmt.Errorf("strategy %q: %w", spec.GetStrategy(), err)
		}
		spec.Env = resolved
	}
	return nil
}
