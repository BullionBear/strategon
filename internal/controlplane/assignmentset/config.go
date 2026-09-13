package assignmentset

import (
	"fmt"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// ConfigWant is one member's catalog lookup after inheritance and expand.
// Fallback is non-empty if and only if the name is implicit; callers then
// try Fallback@version when Primary is missing. An empty Version means this
// member has no config.
type ConfigWant struct {
	Primary  string
	Fallback string
	Version  string
}

// WantConfig picks the config artifact name and version for members[idx].
//
// Name is the first non-empty of member.config, spec.config, then
// {artName}-config. Version is the first non-empty of member.config_version
// and spec.config_version. spec.config and member.config share the same
// expander and value table (${set.name}, ${member.name}, ${member.machine},
// ${member.vars.*}); ${peers} is not available.
//
// An explicit name (set or member) does not fall back to {art}-config /
// {strategy}-config. An explicit name that expands empty, or that has no
// version after coalesce, is an error.
func WantConfig(set *pb.AssignmentSet, idx int, artName string) (ConfigWant, error) {
	members := set.GetSpec().GetMembers()
	if idx < 0 || idx >= len(members) {
		return ConfigWant{}, fmt.Errorf("member index %d out of range", idx)
	}
	self := members[idx]
	spec := set.GetSpec()
	vals := configNameValues(set.GetMetadata().GetName(), self)

	raw := self.GetConfig()
	where := fmt.Sprintf("members[%d].config", idx)
	if raw == "" {
		raw = spec.GetConfig()
		where = "spec.config"
	}

	var name string
	if raw != "" {
		var err error
		name, err = expand(raw, vals, where)
		if err != nil {
			return ConfigWant{}, err
		}
		if name == "" {
			return ConfigWant{}, fmt.Errorf("%s: expands to an empty config name", where)
		}
	}

	version := self.GetConfigVersion()
	if version == "" {
		version = spec.GetConfigVersion()
	}

	if version == "" {
		if raw != "" {
			return ConfigWant{}, fmt.Errorf("%s: config name requires a version", where)
		}
		return ConfigWant{}, nil
	}
	if name != "" {
		return ConfigWant{Primary: name, Version: version}, nil
	}

	strategy := spec.GetStrategy()
	if strategy == "" {
		strategy = artName
	}
	primary := artName + "-config"
	if artName == "" {
		primary = strategy + "-config"
	}
	return ConfigWant{
		Primary:  primary,
		Fallback: strategy + "-config",
		Version:  version,
	}, nil
}

// configNameValues is the substitution table for config artifact names.
// Same keys as memberValues except peers — a peer list is not a catalog name.
func configNameValues(setName string, self *pb.SetMember) map[string]string {
	vals := map[string]string{
		"set.name":       setName,
		"member.name":    self.GetName(),
		"member.machine": self.GetMachine(),
	}
	for k, v := range self.GetVars() {
		vals["member.vars."+k] = v
	}
	return vals
}
