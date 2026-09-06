package driver

import (
	"strings"
	"testing"
)

func TestBuildInitArgsOmitsEnvValues(t *testing.T) {
	spec := StartSpec{
		Rootfs:     "/tmp/rootfs",
		WorkBind:   "/tmp/work",
		SharedBind: "/tmp/shared",
		WorkDir:    "/tmp/work",
		ConfigBind: "/tmp/current/config.yml",
		Argv:       []string{"python", "-m", "app"},
		Env:        []string{"API_KEY=super-secret", "PATH=/usr/bin"},
	}
	args := BuildInitArgs(spec)
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "super-secret") || strings.Contains(joined, "API_KEY") {
		t.Fatalf("init args leaked env: %v", args)
	}
	if args[0] != flagOCIInit {
		t.Fatalf("first arg = %q", args[0])
	}
	var sawSep bool
	for _, a := range args {
		if a == "--" {
			sawSep = true
		}
	}
	if !sawSep {
		t.Fatal("expected -- before argv")
	}
}

func TestParseInitFlag(t *testing.T) {
	in := BuildInitArgs(StartSpec{
		Rootfs: "/r", WorkBind: "/w", SharedBind: "/s", WorkDir: "/w",
		Argv: []string{"sleep", "1"},
	})
	got, err := parseInitFlag(in[1:]) // drop --oci-init
	if err != nil {
		t.Fatal(err)
	}
	if got.Rootfs != "/r" || got.Work != "/w" || len(got.Argv) != 2 || got.Argv[0] != "sleep" {
		t.Fatalf("%+v", got)
	}
}
