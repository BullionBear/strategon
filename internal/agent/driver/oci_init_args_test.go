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

func TestBuildInitArgsVolumes(t *testing.T) {
	args := BuildInitArgs(StartSpec{
		Rootfs:     "/r",
		WorkBind:   "/w",
		SharedBind: "/s",
		WorkDir:    "/w",
		VolumeBinds: []VolumeBind{
			{Host: "/base/volumes/data", Container: "/var/lib/mftik"},
		},
		Argv: []string{"true"},
	})
	var saw string
	for _, a := range args {
		if strings.HasPrefix(a, flagOCIVolume+"=") {
			saw = a
		}
	}
	if saw != flagOCIVolume+"=/base/volumes/data:/var/lib/mftik" {
		t.Fatalf("volume flag = %q args=%v", saw, args)
	}
	got, err := parseInitFlag(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Host != "/base/volumes/data" || got.Volumes[0].Container != "/var/lib/mftik" {
		t.Fatalf("%+v", got)
	}
}

func TestSplitVolumeBindRejectsColonInPaths(t *testing.T) {
	if _, _, ok := splitVolumeBind("/tmp/foo:bar:/data"); ok {
		t.Fatal("colon in host should fail")
	}
	if _, _, ok := splitVolumeBind("relative:/data"); ok {
		t.Fatal("relative host should fail")
	}
}
