// Package assignmentset expands an AssignmentSet's member template into the
// per-machine values the controller writes onto a StrategyAssignmentSpec.
//
// The control plane substitutes only its own namespaced placeholders and
// leaves the agent's (${CONFIG}, ${BINARY}, ${RELEASE_DIR}) verbatim, so the
// two expansion stages cannot collide. Anything else is rejected at apply time
// instead of surfacing later as an agent start failure.
package assignmentset

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// placeholderRE matches ${...}; the body is validated separately.
var placeholderRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// agentPlaceholders are expanded by the agent, not here. They pass through.
var agentPlaceholders = map[string]bool{
	"CONFIG":      true,
	"BINARY":      true,
	"RELEASE_DIR": true,
}

// Expanded is one member's rendered template.
type Expanded struct {
	Args     []string
	Env      map[string]string
	Endpoint string
}

// Expand renders the template for members[idx].
func Expand(set *pb.AssignmentSet, idx int) (*Expanded, error) {
	members := set.GetSpec().GetMembers()
	if idx < 0 || idx >= len(members) {
		return nil, fmt.Errorf("member index %d out of range", idx)
	}
	self := members[idx]
	tmpl := set.GetSpec().GetTemplate()

	peers, err := renderPeers(set, idx)
	if err != nil {
		return nil, err
	}
	vals := memberValues(set.GetMetadata().GetName(), self, peers)

	args, err := expandArgs(tmpl.GetArgs(), vals)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	if len(tmpl.GetEnv()) > 0 {
		env = make(map[string]string, len(tmpl.GetEnv()))
		for k, v := range tmpl.GetEnv() {
			out, err := expand(v, vals, fmt.Sprintf("env %q", k))
			if err != nil {
				return nil, err
			}
			env[k] = out
		}
	}
	endpoint, err := expand(tmpl.GetReadiness().GetEndpoint(), vals, "readiness endpoint")
	if err != nil {
		return nil, err
	}
	return &Expanded{Args: args, Env: env, Endpoint: endpoint}, nil
}

// Validate renders every member so a bad placeholder is a FailedPrecondition at
// apply time rather than a start failure on a machine.
func Validate(set *pb.AssignmentSet) error {
	for i := range set.GetSpec().GetMembers() {
		if _, err := Expand(set, i); err != nil {
			return err
		}
	}
	return nil
}

// memberValues is the substitution table for one member.
func memberValues(setName string, self *pb.SetMember, peers string) map[string]string {
	vals := map[string]string{
		"set.name":       setName,
		"member.name":    self.GetName(),
		"member.machine": self.GetMachine(),
		"peers":          peers,
	}
	for k, v := range self.GetVars() {
		vals["member.vars."+k] = v
	}
	return vals
}

// renderPeers formats every other member and joins them. Members are rendered
// in spec order, then sorted, so the result does not depend on map iteration
// and an unchanged set does not churn the assignment's generation.
func renderPeers(set *pb.AssignmentSet, selfIdx int) (string, error) {
	pl := set.GetSpec().GetTemplate().GetPeers()
	if pl.GetFormat() == "" {
		return "", nil
	}
	sep := pl.GetSeparator()
	if sep == "" {
		sep = ","
	}
	var out []string
	for i, m := range set.GetSpec().GetMembers() {
		if i == selfIdx && !pl.GetIncludeSelf() {
			continue
		}
		vals := map[string]string{
			"peer.name":    m.GetName(),
			"peer.machine": m.GetMachine(),
		}
		for k, v := range m.GetVars() {
			vals["peer.vars."+k] = v
		}
		s, err := expand(pl.GetFormat(), vals, "peers.format")
		if err != nil {
			return "", err
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, sep), nil
}

// expandArgs renders each arg in place. Nothing is added, reordered or
// dropped: an arg that expands to the empty string is passed through as an
// empty argument.
//
// An earlier version dropped empty args and the bare flag before them, so a
// single-member set would not pass "--routes" with no value. That inferred a
// flag/value pairing from adjacency that the manifest never declares, and it
// ate standalone flags: ["--dry-run", "${member.vars.mode}"] with an empty
// mode lost --dry-run too. It also turned out to be unnecessary — nats-server
// starts cleanly with --routes "" — so the engine no longer guesses. Whether
// an empty value is acceptable is the workload's business, and therefore the
// manifest's.
func expandArgs(raw []string, vals map[string]string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]string, len(raw))
	for i, a := range raw {
		rendered, err := expand(a, vals, fmt.Sprintf("args[%d]", i))
		if err != nil {
			return nil, err
		}
		out[i] = rendered
	}
	return out, nil
}

// expand substitutes control-plane placeholders and passes agent ones through.
func expand(s string, vals map[string]string, where string) (string, error) {
	if s == "" {
		return "", nil
	}
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		name := match[2 : len(match)-1]
		if agentPlaceholders[name] {
			return match // the agent expands this later
		}
		v, ok := vals[name]
		if !ok {
			firstErr = fmt.Errorf("%s: unknown placeholder ${%s}", where, name)
			return match
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// ValidateMemberName rejects names that cannot be a WorkDir segment
// (<base>/<name>). Same rules as the agent's strategy-name check.
// Callers must persist the trimmed name; this function does not rewrite.
func ValidateMemberName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty member name")
	}
	// "." joins to the agent base directory; ".." and slashes escape it.
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid member name %q", name)
	}
	return nil
}
