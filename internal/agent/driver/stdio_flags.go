package driver

import "strconv"

const (
	flagStdioTee     = "--stdio-tee"
	flagStdioLogDir  = "--log-dir"
	flagStdioVersion = "--log-version"
)

const (
	// flagRlimitNofile may precede any helper flag: the helper lowers
	// RLIMIT_NOFILE (soft and hard) before doing anything else, so the
	// payload it runs inherits the limit.
	flagRlimitNofile = "--rlimit-nofile"
	// flagExecPayload execs the argv after "--" in place: the shim EXEC uses
	// when only an rlimit, not stdio capture, needs this binary in front.
	flagExecPayload = "--exec-payload"
	// flagSealCgroup remounts the host cgroupfs read-only in the child's
	// mount namespace, then acks on fd 3, before --exec-payload or
	// --stdio-tee. It follows --rlimit-nofile when both are set.
	flagSealCgroup = "--seal-cgroup"
)

// withRlimitArgs prefixes helper args with the max_open_files flag when set.
func withRlimitArgs(spec StartSpec, args []string) []string {
	if spec.MaxOpenFiles <= 0 {
		return args
	}
	return append([]string{flagRlimitNofile + "=" + strconv.Itoa(int(spec.MaxOpenFiles))}, args...)
}
