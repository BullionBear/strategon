package store

import (
	"sort"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// sortArtifactsByNameThenNewest orders by name ascending, then created_at
// descending (newest / "latest" first within a name). Version string is a
// tie-breaker only — latest is defined by registration time, not semver.
func sortArtifactsByNameThenNewest(out []*pb.ArtifactRef) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetName() != out[j].GetName() {
			return out[i].GetName() < out[j].GetName()
		}
		ti := artifactCreatedUnix(out[i])
		tj := artifactCreatedUnix(out[j])
		if ti != tj {
			return ti > tj
		}
		return out[i].GetVersion() < out[j].GetVersion()
	})
}

func sortArtifactRecordsByNameThenNewest(out []*ArtifactRecord) {
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].Ref, out[j].Ref
		if ai.GetName() != aj.GetName() {
			return ai.GetName() < aj.GetName()
		}
		ti := artifactCreatedUnix(ai)
		tj := artifactCreatedUnix(aj)
		if ti != tj {
			return ti > tj
		}
		return ai.GetVersion() < aj.GetVersion()
	})
}

func artifactCreatedUnix(ref *pb.ArtifactRef) int64 {
	if ref == nil || ref.GetCreatedAt() == nil {
		return 0
	}
	return ref.GetCreatedAt().AsTime().UnixNano()
}
