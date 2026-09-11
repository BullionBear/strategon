package assignmentset

import (
	"strings"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// natsLike is the shape examples/nats/cluster.yaml produces: the control plane
// holds no NATS knowledge, so this manifest is the only place the names appear.
func natsLike(machines ...string) *pb.AssignmentSet {
	set := &pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			Strategy: "nats",
			Template: &pb.MemberTemplate{
				Args: []string{"-c", "${CONFIG}", "--routes", "${peers}"},
				Env: map[string]string{
					"NATS_SERVER_NAME":  "${member.name}",
					"NATS_CLUSTER_NAME": "${set.name}",
					"NATS_CLUSTER_PORT": "${member.vars.cluster_port}",
				},
				Readiness: &pb.ReadinessProbe{
					Endpoint: "http://127.0.0.1:${member.vars.monitor_port}/healthz",
				},
				Peers: &pb.PeerList{
					Format: "nats://${peer.vars.route_host}:${peer.vars.cluster_port}",
				},
			},
		},
	}
	for i, m := range machines {
		set.Spec.Members = append(set.Spec.Members, &pb.SetMember{
			Machine: m,
			Name:    "nats-" + m,
			Vars: map[string]string{
				"route_host":   "10.0.0." + string(rune('1'+i)),
				"cluster_port": "6222",
				"monitor_port": "8222",
			},
		})
	}
	return set
}

func TestExpandNatsManifest(t *testing.T) {
	set := natsLike("m1", "m2", "m3")
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", "${CONFIG}", "--routes", "nats://10.0.0.2:6222,nats://10.0.0.3:6222"}
	if strings.Join(got.Args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q, want %q", got.Args, want)
	}
	if got.Env["NATS_SERVER_NAME"] != "nats-m1" {
		t.Fatalf("server name = %q", got.Env["NATS_SERVER_NAME"])
	}
	if got.Env["NATS_CLUSTER_NAME"] != "trading" {
		t.Fatalf("cluster name = %q", got.Env["NATS_CLUSTER_NAME"])
	}
	if got.Endpoint != "http://127.0.0.1:8222/healthz" {
		t.Fatalf("endpoint = %q", got.Endpoint)
	}
}

// ${CONFIG} must survive expansion: the agent resolves it to the materialised
// config artifact, and eating it here would break every deployment.
func TestExpandLeavesAgentPlaceholders(t *testing.T) {
	set := natsLike("m1", "m2")
	set.Spec.Template.Args = []string{"${BINARY}", "-c", "${CONFIG}", "-d", "${RELEASE_DIR}"}
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"${BINARY}", "${CONFIG}", "${RELEASE_DIR}"} {
		found := false
		for _, a := range got.Args {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was consumed; args = %q", want, got.Args)
		}
	}
}

func TestExpandNestedVolumePlaceholder(t *testing.T) {
	set := natsLike("m1")
	set.Spec.Template.VolumeMounts = []*pb.VolumeMount{
		{Name: "${member.name}-data", ContainerPath: "/var/lib/mftik"},
	}
	set.Spec.Template.Env["MFTIK_DATA"] = "${VOLUME:${member.name}-data}"
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.VolumeMounts) != 1 || got.VolumeMounts[0].GetName() != "nats-m1-data" {
		t.Fatalf("mounts = %+v", got.VolumeMounts)
	}
	if got.Env["MFTIK_DATA"] != "${VOLUME:nats-m1-data}" {
		t.Fatalf("env = %q", got.Env["MFTIK_DATA"])
	}
}

func TestExpandSelfReferentialVarDoesNotHang(t *testing.T) {
	set := natsLike("m1")
	set.Spec.Members[0].Vars["x"] = "${member.vars.x}"
	set.Spec.Template.Args = []string{"${member.vars.x}"}
	done := make(chan error, 1)
	var got *Expanded
	go func() {
		var err error
		got, err = Expand(set, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if got.Args[0] != "${member.vars.x}" {
			t.Fatalf("args = %q", got.Args)
		}
	case <-time.After(time.Second):
		t.Fatal("expand hung on self-referential member var")
	}
}

func TestExpandMutualVarsDoNotHang(t *testing.T) {
	set := natsLike("m1")
	set.Spec.Members[0].Vars["x"] = "a${member.vars.y}"
	set.Spec.Members[0].Vars["y"] = "${member.vars.x}"
	set.Spec.Template.Args = []string{"${member.vars.x}"}
	done := make(chan error, 1)
	go func() {
		_, err := Expand(set, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("expand hung on mutually referential member vars")
	}
}

func TestExpandLeavesLiteralPlaceholderInVarValue(t *testing.T) {
	set := natsLike("m1")
	set.Spec.Members[0].Vars["home"] = "${HOME}"
	set.Spec.Template.Args = []string{"${member.vars.home}"}
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Args[0] != "${HOME}" {
		t.Fatalf("args = %q, want ${HOME} passed through", got.Args)
	}
}

// A one-member set has no peers, so ${peers} renders empty and is passed
// through as an empty argument. Verified against nats-server 2.10.29: with a
// cluster configured, `--routes ""` starts cleanly, keeps cluster mode, and
// listens for route connections — it simply has no peers to dial yet.
//
// The engine deliberately does not drop the empty value or the --routes flag
// before it. Whether an empty value is acceptable belongs to the workload, and
// guessing at flag/value pairing ate standalone flags.
func TestExpandSingleMemberYieldsEmptyPeers(t *testing.T) {
	got, err := Expand(natsLike("m1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", "${CONFIG}", "--routes", ""}
	if strings.Join(got.Args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q, want %q", got.Args, want)
	}
}

// The engine must not infer that an arg belongs to the flag before it. This is
// the case the old drop heuristic got wrong.
func TestExpandKeepsStandaloneFlagBeforeEmptyValue(t *testing.T) {
	set := natsLike("m1", "m2")
	set.Spec.Template.Args = []string{"--dry-run", "${member.vars.mode}", "run"}
	set.Spec.Members[0].Vars["mode"] = ""
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dry-run", "", "run"}
	if strings.Join(got.Args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q, want %q", got.Args, want)
	}
}

// Peer order must not depend on map iteration, or the spec would churn.
func TestExpandPeerOrderStable(t *testing.T) {
	set := natsLike("m1", "m2", "m3")
	first, err := Expand(set, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, err := Expand(set, 1)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(next.Args, "|") != strings.Join(first.Args, "|") {
			t.Fatalf("peer order unstable: %q vs %q", next.Args, first.Args)
		}
	}
}

func TestExpandIncludeSelf(t *testing.T) {
	set := natsLike("m1", "m2")
	set.Spec.Template.Peers.IncludeSelf = true
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Args[3], "10.0.0.1") {
		t.Fatalf("include_self did not add self: %q", got.Args[3])
	}
}

func TestExpandSeparator(t *testing.T) {
	set := natsLike("m1", "m2", "m3")
	set.Spec.Template.Peers.Separator = " "
	got, err := Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Args[3] != "nats://10.0.0.2:6222 nats://10.0.0.3:6222" {
		t.Fatalf("separator ignored: %q", got.Args[3])
	}
}

func TestValidateRejectsUnknownPlaceholder(t *testing.T) {
	cases := map[string]func(*pb.AssignmentSet){
		"args":      func(s *pb.AssignmentSet) { s.Spec.Template.Args = []string{"${member.nmae}"} },
		"env":       func(s *pb.AssignmentSet) { s.Spec.Template.Env = map[string]string{"X": "${nope}"} },
		"readiness": func(s *pb.AssignmentSet) { s.Spec.Template.Readiness.Endpoint = "${bad}" },
		"peers":     func(s *pb.AssignmentSet) { s.Spec.Template.Peers.Format = "${peer.vars}${oops}" },
		"var typo":  func(s *pb.AssignmentSet) { s.Spec.Template.Args = []string{"${member.vars.missing_ok}"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			set := natsLike("m1", "m2")
			mutate(set)
			err := Validate(set)
			if name == "var typo" {
				// An absent var is still an unknown placeholder: vars are only
				// defined by what the member actually declares.
				if err == nil {
					t.Fatal("missing var should be rejected")
				}
				return
			}
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), "unknown placeholder") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateAcceptsExampleShape(t *testing.T) {
	if err := Validate(natsLike("m1", "m2", "m3")); err != nil {
		t.Fatalf("example manifest rejected: %v", err)
	}
}

func TestValidateMemberName(t *testing.T) {
	if err := ValidateMemberName("nats-m1"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "nats/m1", "a..b", `nats\m1`, "   "} {
		if err := ValidateMemberName(bad); err == nil {
			t.Fatalf("ValidateMemberName(%q) should fail", bad)
		}
	}
}
