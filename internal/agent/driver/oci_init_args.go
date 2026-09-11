package driver

import (
	"fmt"
	"strings"
)

const (
	flagOCIInit   = "--oci-init"
	flagOCIProbe  = "--oci-probe"
	flagOCIRootfs = "--oci-rootfs"
	flagOCIWork   = "--oci-work"
	flagOCIShared = "--oci-shared"
	flagOCIConfig = "--oci-config"
	flagOCICWD    = "--oci-cwd"
	flagOCIVolume = "--oci-volume"
)

// InitArgs is the non-secret flag set passed to --oci-init. Env stays on
// cmd.Env and must never appear here.
type InitArgs struct {
	Rootfs  string
	Work    string
	Shared  string
	Config  string
	CWD     string
	Volumes []VolumeBind
	Argv    []string
}

// BuildInitArgs returns the argv for a re-exec of this binary as --oci-init.
// spec.Env values must not appear in the result.
func BuildInitArgs(spec StartSpec) []string {
	args := []string{
		flagOCIInit,
		flagOCIRootfs + "=" + spec.Rootfs,
		flagOCIWork + "=" + spec.WorkBind,
		flagOCIShared + "=" + spec.SharedBind,
		flagOCICWD + "=" + spec.WorkDir,
	}
	if spec.ConfigBind != "" {
		args = append(args, flagOCIConfig+"="+spec.ConfigBind)
	}
	for _, b := range spec.VolumeBinds {
		args = append(args, flagOCIVolume+"="+b.Host+":"+b.Container)
	}
	if len(spec.Argv) > 0 {
		args = append(args, "--")
		args = append(args, spec.Argv...)
	}
	return args
}

func parseInitFlag(args []string) (InitArgs, error) {
	out := InitArgs{}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			out.Argv = append([]string(nil), args[i+1:]...)
			return out, nil
		}
		key, val, ok := splitFlag(a)
		if !ok {
			return InitArgs{}, fmt.Errorf("oci-init: unexpected arg %q", a)
		}
		switch key {
		case flagOCIRootfs:
			out.Rootfs = val
		case flagOCIWork:
			out.Work = val
		case flagOCIShared:
			out.Shared = val
		case flagOCIConfig:
			out.Config = val
		case flagOCICWD:
			out.CWD = val
		case flagOCIVolume:
			host, container, ok := splitVolumeBind(val)
			if !ok {
				return InitArgs{}, fmt.Errorf("oci-init: invalid %s=%s", flagOCIVolume, val)
			}
			out.Volumes = append(out.Volumes, VolumeBind{Host: host, Container: container})
		default:
			return InitArgs{}, fmt.Errorf("oci-init: unknown flag %q", key)
		}
		i++
	}
	return out, nil
}

func splitVolumeBind(s string) (host, container string, ok bool) {
	i := strings.Index(s, ":/")
	if i <= 0 {
		return "", "", false
	}
	host, container = s[:i], s[i+1:]
	if !strings.HasPrefix(host, "/") || !strings.HasPrefix(container, "/") {
		return "", "", false
	}
	if strings.Contains(host, ":") || strings.Contains(container[1:], ":") {
		return "", "", false
	}
	return host, container, true
}

func splitFlag(s string) (key, val string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
