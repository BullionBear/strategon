//go:build linux

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	attr := ociSysProcAttr(0, 0, false)
	want := uintptr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS)
	if attr.Cloneflags != want {
		t.Fatalf("cloneflags = %#x, want %#x", attr.Cloneflags, want)
	}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0].HostID != os.Getuid() || attr.UidMappings[0].Size != 1 {
		t.Fatalf("uid mappings = %#v, want a single-uid map onto this process", attr.UidMappings)
	}
}

func TestHostPIDProbeMatchesStart(t *testing.T) {
	attr := ociSysProcAttr(0, 0, true)
	want := uintptr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWUTS)
	if attr.Cloneflags != want {
		t.Fatalf("cloneflags = %#x, want %#x", attr.Cloneflags, want)
	}
	if attr.Cloneflags&unix.CLONE_NEWPID != 0 {
		t.Fatal("host-pid clone set still has CLONE_NEWPID")
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

func TestOCIDriverCaptureStdio(t *testing.T) {
	requireUserNS(t)
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not available")
	}
	rootfs := t.TempDir()
	copyIntoRootfs(t, rootfs, echo)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(filepath.Dir(work), StdioDirName)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy:       "s",
		Driver:         KindOCI,
		Rootfs:         rootfs,
		Argv:           []string{echo, "oci-hi"},
		WorkDir:        work,
		WorkBind:       work,
		Env:            []string{"PATH=/bin:/usr/bin"},
		CaptureStdio:   true,
		PayloadLogDir:  logDir,
		PayloadVersion: "v1",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	info := d.WatchExit(p, time.Now)
	if info.Code != 0 {
		body, _ := os.ReadFile(OCIInitLogPath(work))
		t.Fatalf("exit %d initlog=%s", info.Code, body)
	}
	got, err := os.ReadFile(filepath.Join(logDir, PayloadLogName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "oci-hi") {
		t.Fatalf("log = %q", got)
	}
}

func TestOCIDriverCaptureOffDiscards(t *testing.T) {
	requireUserNS(t)
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not available")
	}
	rootfs := t.TempDir()
	copyIntoRootfs(t, rootfs, echo)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(filepath.Dir(work), StdioDirName)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy:      "s",
		Driver:        KindOCI,
		Rootfs:        rootfs,
		Argv:          []string{echo, "oci-discard"},
		WorkDir:       work,
		WorkBind:      work,
		Env:           []string{"PATH=/bin:/usr/bin"},
		PayloadLogDir: logDir,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	info := d.WatchExit(p, time.Now)
	if info.Code != 0 {
		body, _ := os.ReadFile(OCIInitLogPath(work))
		t.Fatalf("exit %d initlog=%s", info.Code, body)
	}
	if _, err := os.Stat(filepath.Join(logDir, PayloadLogName)); !os.IsNotExist(err) {
		t.Fatalf("capture off must not write payload.log: %v", err)
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

func requireHostPID(t *testing.T) {
	t.Helper()
	requireUserNS(t)
	if HostPIDAvailable() {
		return
	}
	if os.Getenv("STRATEGON_REQUIRE_USERNS") != "" {
		t.Fatal("host-pid OCI unavailable but STRATEGON_REQUIRE_USERNS is set")
	}
	t.Skip("host-pid OCI unavailable")
}

func TestOCIHostPIDGrandchildSurvivesGroupKill(t *testing.T) {
	requireHostPID(t)
	grand := startOCIGrandchild(t, true)
	if !procRunning(grand) {
		t.Fatalf("grandchild %d died with the supervised process", grand)
	}
}

func TestOCIPrivatePIDKillsGrandchild(t *testing.T) {
	requireUserNS(t)
	grand := startOCIGrandchild(t, false)
	deadline := time.Now().Add(2 * time.Second)
	for procRunning(grand) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procRunning(grand) {
		t.Fatalf("grandchild %d still alive after private pid namespace init died", grand)
	}
}

func TestOCIHostPIDSeesHostProc(t *testing.T) {
	requireHostPID(t)
	if got := hostProcAnswer(t, true); got != "yes" {
		t.Fatalf("host proc visible = %q, want yes", got)
	}
}

func TestOCIPrivatePIDHidesHostProc(t *testing.T) {
	requireUserNS(t)
	if os.Getpid() < 10 {
		t.Skip("test pid is too small to distinguish from the container's pid 1")
	}
	if got := hostProcAnswer(t, false); got != "no" {
		t.Fatalf("host proc visible = %q, want no", got)
	}
}

func startOCIGrandchild(t *testing.T, hostPID bool) int {
	t.Helper()
	sh, setsid, sleep, rootfs, work := ociToolRootfs(t)
	// Relative path: oci-init chdirs to work before covering /tmp with tmpfs,
	// and test work dirs live under /tmp. An absolute /tmp/... path would
	// land on that tmpfs instead of the bind.
	body := fmt.Sprintf("#!%s\n%s %s 60 &\necho $! > grand.pid\n%s 120\n", sh, setsid, sleep, sleep)
	if err := os.WriteFile(filepath.Join(rootfs, "stay.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy:   "s",
		Driver:     KindOCI,
		Rootfs:     rootfs,
		Argv:       []string{"/stay.sh"},
		WorkDir:    work,
		WorkBind:   work,
		Env:        []string{"PATH=/usr/bin:/bin"},
		OCIHostPID: hostPID,
	}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pidText := waitFile(t, filepath.Join(work, "grand.pid"), OCIInitLogPath(work), p.PID)
	inner, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil || inner <= 0 {
		t.Fatalf("grand pid %q: %v", pidText, err)
	}
	// stay.sh records $! inside the container. With a private PID namespace
	// that number is not a host pid; /proc/<n> on the host is some other
	// process (CI pid 6 is a runner daemon). Resolve it before killing the
	// namespace init, while both processes are still alive.
	grand, err := hostPIDForInnerPID(inner, p.PID)
	if err != nil {
		body, _ := os.ReadFile(OCIInitLogPath(work))
		t.Fatalf("resolve grandchild host pid for inner pid %d anchor %d: %v\n%s", inner, p.PID, err, body)
	}
	t.Cleanup(func() { _ = syscall.Kill(grand, syscall.SIGKILL) })
	if err := d.Signal(p, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	d.WatchExit(p, time.Now)
	return grand
}

// hostPIDForInnerPID maps a pid observed inside anchor's PID namespace to the
// host pid. NSpid's last field is that innermost pid; the pid namespace inode
// keeps a same-numbered process in another namespace from matching.
func hostPIDForInnerPID(inner, anchor int) (int, error) {
	anchorNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", anchor))
	if err != nil {
		return 0, fmt.Errorf("anchor pid namespace: %w", err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	var match, found int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		ids := nspids(string(status))
		if len(ids) == 0 || ids[len(ids)-1] != inner {
			continue
		}
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
		if err != nil || ns != anchorNS {
			continue
		}
		match = pid
		found++
	}
	if found != 1 {
		return 0, fmt.Errorf("found %d processes with inner pid %d in %s", found, inner, anchorNS)
	}
	return match, nil
}

func nspids(status string) []int {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(line)
		ids := make([]int, 0, len(fields)-1)
		for _, f := range fields[1:] {
			n, err := strconv.Atoi(f)
			if err != nil {
				return nil
			}
			ids = append(ids, n)
		}
		return ids
	}
	return nil
}

func TestNSpids(t *testing.T) {
	got := nspids("Name:\tsleep\nNSpid:\t4242\t6\nPPid:\t1\n")
	if len(got) != 2 || got[0] != 4242 || got[1] != 6 {
		t.Fatalf("nspids = %v, want [4242 6]", got)
	}
	if got := nspids("Name:\tsh\n"); got != nil {
		t.Fatalf("nspids without NSpid = %v, want nil", got)
	}
}

func hostProcAnswer(t *testing.T, hostPID bool) string {
	t.Helper()
	sh, _, sleep, rootfs, work := ociToolRootfs(t)
	body := fmt.Sprintf("#!%s\nif [ -d /proc/%d ]; then echo yes; else echo no; fi > hostproc\n%s 30\n", sh, os.Getpid(), sleep)
	if err := os.WriteFile(filepath.Join(rootfs, "stay.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy:   "s",
		Driver:     KindOCI,
		Rootfs:     rootfs,
		Argv:       []string{"/stay.sh"},
		WorkDir:    work,
		WorkBind:   work,
		Env:        []string{"PATH=/usr/bin:/bin"},
		OCIHostPID: hostPID,
	}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	text := waitFile(t, filepath.Join(work, "hostproc"), OCIInitLogPath(work), p.PID)
	_ = d.Signal(p, syscall.SIGKILL)
	d.WatchExit(p, time.Now)
	return strings.TrimSpace(string(text))
}

func ociToolRootfs(t *testing.T) (sh, setsid, sleep, rootfs, work string) {
	t.Helper()
	var err error
	sh, err = exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	setsid, err = exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid not available")
	}
	sleep, err = exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	rootfs = t.TempDir()
	copyIntoRootfs(t, rootfs, sh)
	copyIntoRootfs(t, rootfs, setsid)
	copyIntoRootfs(t, rootfs, sleep)
	work = filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return sh, setsid, sleep, rootfs, work
}

func waitFile(t *testing.T, path, initLog string, pid int) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, _ := os.ReadFile(initLog)
	cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	t.Fatalf("timeout waiting for %s; pid=%d cmdline=%q status=%q oci-init log:\n%s", path, pid, cmd, status, body)
	return nil
}

func procRunning(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return false
	}
	state := s[i+2]
	return state != 'Z' && state != 'X'
}
