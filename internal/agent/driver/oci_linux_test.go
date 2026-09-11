//go:build linux

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if MaybeRunOCIHelper() {
		return
	}
	os.Exit(m.Run())
}

// requireUserNS skips locally (a dev box may block user namespaces) but fails
// where STRATEGON_REQUIRE_USERNS is set, so CI cannot go green by skipping the
// only tests that exercise --oci-init.
func requireUserNS(t *testing.T) {
	t.Helper()
	if UserNSAvailable() {
		return
	}
	if os.Getenv("STRATEGON_REQUIRE_USERNS") != "" {
		t.Fatal("unprivileged user namespaces unavailable but STRATEGON_REQUIRE_USERNS is set")
	}
	t.Skip("unprivileged user namespaces unavailable")
}

func TestUserNSAvailableOrSkip(t *testing.T) {
	requireUserNS(t)
}

func TestOCIDriverStartSignalWatch(t *testing.T) {
	requireUserNS(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	rootfs := t.TempDir()
	binDir := filepath.Join(rootfs, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy: "s",
		Driver:   KindOCI,
		Rootfs:   rootfs,
		Argv:     []string{"sleep", "30"},
		WorkDir:  work,
		WorkBind: work,
		Env:      []string{"PATH=/bin"},
	}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if p.PID <= 0 {
		t.Fatalf("bad pid %d", p.PID)
	}

	exited := make(chan ExitInfo, 1)
	go func() { exited <- d.WatchExit(p, time.Now) }()
	if err := d.Signal(p, syscall.SIGKILL); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchExit did not return")
	}
}

func TestOCIDriverRejectsEmptyArgv(t *testing.T) {
	d := NewOCIDriver(NewExecDriver(""))
	if _, err := d.Start(StartSpec{Driver: KindOCI, Rootfs: t.TempDir()}, time.Now()); err == nil {
		t.Fatal("expected error")
	}
}

// The probe is only meaningful if it asks the kernel for exactly what Start
// asks for. A weaker probe (CLONE_NEWUSER alone, or exec'ing /bin/true instead
// of /proc/self/exe) reports OCI-capable on hosts where Start is refused — the
// control plane then admits a deploy that dies at launch.
func TestUserNSProbeMatchesStart(t *testing.T) {
	cmd := probeCommand()
	if cmd.Path != "/proc/self/exe" {
		t.Fatalf("probe execs %q, want /proc/self/exe like Start", cmd.Path)
	}
	if len(cmd.Args) < 2 || cmd.Args[1] != flagOCIProbe {
		t.Fatalf("probe args = %#v, want %s", cmd.Args, flagOCIProbe)
	}
	attr := ociSysProcAttr(0, 0)
	want := uintptr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS)
	if attr.Cloneflags != want {
		t.Fatalf("cloneflags = %#x, want %#x", attr.Cloneflags, want)
	}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0].HostID != os.Getuid() || attr.UidMappings[0].Size != 1 {
		t.Fatalf("uid mappings = %#v, want a single-uid map onto this process", attr.UidMappings)
	}
}

// copyIntoRootfs copies bin into rootfs at the same path, together with any
// shared libraries it needs. Without the libraries the payload cannot exec, and
// a test that only checks the fork succeeded would not notice.
func copyIntoRootfs(t *testing.T, rootfs, bin string) {
	t.Helper()
	copyOne := func(src string) {
		dst := filepath.Join(rootfs, src)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyOne(bin)
	out, err := exec.Command("ldd", bin).Output()
	if err != nil {
		return // static binary, or no ldd: nothing more to carry
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "/") && strings.Contains(f, ".so") {
			resolved, err := filepath.EvalSymlinks(f)
			if err != nil {
				continue
			}
			copyOne(f)
			if resolved != f {
				copyOne(resolved)
			}
		}
	}
}

// The payload must actually reach exec. --oci-init failures now land in
// the sibling oci-init.log (next to, not inside, WorkDir), but a child that
// dies in init is still a successful fork from Start's point of view — the
// test waits to see the payload stay up.
func TestOCIDriverPayloadReachesExec(t *testing.T) {
	requireUserNS(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	rootfs := t.TempDir()
	copyIntoRootfs(t, rootfs, sleep)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy: "s",
		Driver:   KindOCI,
		Rootfs:   rootfs,
		Argv:     []string{sleep, "30"},
		WorkDir:  work,
		WorkBind: work,
		Env:      []string{"PATH=/bin:/usr/bin"},
	}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	exited := make(chan ExitInfo, 1)
	go func() { exited <- d.WatchExit(p, time.Now) }()
	select {
	case info := <-exited:
		body, _ := os.ReadFile(OCIInitLogPath(work))
		t.Fatalf("payload exited during init (code %d); --oci-init failed before exec\nlog: %s", info.Code, body)
	case <-time.After(time.Second):
	}
	if err := d.Signal(p, syscall.SIGKILL); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchExit did not return")
	}
}

func TestOCIDriverInitStderrGoesToWorkLog(t *testing.T) {
	requireUserNS(t)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy: "s",
		Driver:   KindOCI,
		Rootfs:   filepath.Join(t.TempDir(), "no-such-rootfs"),
		Argv:     []string{"/bin/true"},
		WorkDir:  work,
		WorkBind: work,
	}, time.Now())
	if err != nil {
		t.Fatalf("start (fork) should succeed: %v", err)
	}
	info := d.WatchExit(p, time.Now)
	if info.Code == 0 {
		t.Fatal("expected init to fail")
	}
	body, err := os.ReadFile(OCIInitLogPath(work))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "oci-init:") {
		t.Fatalf("log = %q, want oci-init:", body)
	}
	if _, err := os.Stat(filepath.Join(work, OCIInitLogName)); !os.IsNotExist(err) {
		t.Fatalf("log must not sit inside the bind-mounted work dir, err=%v", err)
	}
}

func TestWriteExecFailure(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "oci-init")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	writeExecFailure(int(f.Fd()), nil, "/payload", syscall.ENOEXEC)
	body, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "oci-init: exec /payload") {
		t.Fatalf("log = %q, want oci-init: exec /payload", body)
	}
}

func TestWriteExecFailureSkipsOnDupError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "oci-init")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	writeExecFailure(int(f.Fd()), syscall.EBADF, "/payload", syscall.ENOEXEC)
	body, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("dup failure must not write, got %q", body)
	}
}

// After discardStdio, unix.Exec failures used to vanish into /dev/null.
// A non-ELF +x payload reaches lookPath + discardStdio, then Exec fails.
func TestOCIDriverExecFailureReachesInitLog(t *testing.T) {
	requireUserNS(t)
	rootfs := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootfs, "payload"), []byte("not-an-elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy: "s",
		Driver:   KindOCI,
		Rootfs:   rootfs,
		Argv:     []string{"/payload"},
		WorkDir:  work,
		WorkBind: work,
	}, time.Now())
	if err != nil {
		t.Fatalf("start (fork) should succeed: %v", err)
	}
	info := d.WatchExit(p, time.Now)
	if info.Code == 0 {
		t.Fatal("expected exec to fail")
	}
	body, err := os.ReadFile(OCIInitLogPath(work))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "oci-init: exec") {
		t.Fatalf("log = %q, want oci-init: exec (discardStdio must not swallow exec errors)", body)
	}
}
