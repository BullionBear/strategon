//go:build linux

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// cgroupUnitSeq keeps TestCgroupSlotLimitsAndOOM's unit directory unique
// across repeated runs in one process. -count reuses the pid, and a unit
// that already has subtree_control cannot accept the stray process.
var cgroupUnitSeq atomic.Uint64

func testCgroupUnit() string {
	return fmt.Sprintf("unit-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), cgroupUnitSeq.Add(1))
}

func TestExecLimitsWithoutCgroupRootFailStart(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	d := NewExecDriver("")
	for _, spec := range []StartSpec{
		{Strategy: "s", BinaryPath: sleep, Args: []string{"30"}, MemoryBytes: 64 << 20},
		{Strategy: "s", BinaryPath: sleep, Args: []string{"30"}, CPUMillicores: 500},
	} {
		p, err := d.Start(spec, time.Now())
		if err == nil {
			_ = d.Signal(p, syscall.SIGKILL)
			t.Fatal("limits without a cgroup root must fail the start, not run unconfined")
		}
		if !strings.Contains(err.Error(), "cgroup root") {
			t.Fatalf("error should name the missing cgroup root: %v", err)
		}
	}
}

// max_open_files lands on the payload pid itself, both through the
// --exec-payload shim and through the stdio tee.
func TestExecMaxOpenFiles(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	d := NewExecDriver("")
	for name, spec := range map[string]StartSpec{
		"shim": {Strategy: "s", BinaryPath: sleep, Args: []string{"30"}, MaxOpenFiles: 777},
		"tee": {Strategy: "s", BinaryPath: sleep, Args: []string{"30"}, MaxOpenFiles: 777,
			CaptureStdio: true, PayloadLogDir: t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := d.Start(spec, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Signal(p, syscall.SIGKILL); d.WatchExit(p, time.Now) }()
			pid := p.PID
			if name == "tee" {
				pid = waitChild(t, p.PID)
			} else {
				waitComm(t, pid, "sleep")
			}
			if got := nofileLimit(t, pid); got != "777 777" {
				t.Fatalf("Max open files = %q, want soft and hard 777", got)
			}
		})
	}
}

func TestExecMaxOpenFilesAboveHardLimitFails(t *testing.T) {
	d := NewExecDriver("")
	_, err := d.Start(StartSpec{Strategy: "s", BinaryPath: "/bin/true", MaxOpenFiles: 1<<31 - 1}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "hard limit") {
		t.Fatalf("want hard-limit rejection before fork, got %v", err)
	}
}

// TestCgroupSlotLimitsAndOOM runs only in a disposable privileged container
// (STRATEGON_CGROUP_TEST=1, --privileged --cgroupns=private): it rearranges
// the container's cgroup tree the way systemd Delegate= + DelegateSubgroup=
// would, then checks placement, limits and the oom_kill counter.
func TestCgroupSlotLimitsAndOOM(t *testing.T) {
	if os.Getenv("STRATEGON_CGROUP_TEST") == "" {
		t.Skip("set STRATEGON_CGROUP_TEST=1 in a privileged container")
	}
	// The namespace root holds every process; move them out so it can
	// delegate memory and cpu, as a systemd slice does for the unit. Other
	// test binaries (go test ./...) keep spawning into it, so retry.
	unit := testCgroupUnit()
	for _, d := range []string{"init", unit + "/agent"} {
		if err := os.MkdirAll(filepath.Join(cgroupFS, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; ; attempt++ {
		if err := evacuateCgroup(cgroupFS, filepath.Join(cgroupFS, "init")); err != nil {
			t.Fatal(err)
		}
		err := enableControllers(cgroupFS)
		if err == nil {
			break
		}
		if attempt == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A leftover payload in the unit cgroup, as after a pre-DelegateSubgroup agent.
	stray := exec.Command("sleep", "30")
	if err := stray.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stray.Process.Kill(); _ = stray.Wait() }()
	if err := os.WriteFile(filepath.Join(cgroupFS, unit, "cgroup.procs"), []byte(strconv.Itoa(stray.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupFS, unit, "agent/cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := ResolveCgroupRoot(CgroupRootAuto)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cgroupFS, unit, "strategies"); root != want {
		t.Fatalf("auto root = %s, want %s", root, want)
	}
	if err := SetupCgroupRoot(root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cgroupFS, unit, cgroupUnassigned, "cgroup.procs")); !strings.Contains(string(b), strconv.Itoa(stray.Process.Pid)) {
		t.Fatalf("stray payload should be moved to %s, have %q", cgroupUnassigned, b)
	}

	d := NewExecDriver(root)
	if err := d.CheckLimits(StartSpec{Strategy: "slot", CPUMillicores: 5}); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("cpu 5 must be rejected before any probe write: %v", err)
	}
	if err := d.CheckLimits(StartSpec{Strategy: "slot", CPUMillicores: 500, MemoryBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	p, err := d.Start(StartSpec{Strategy: "slot", BinaryPath: "/bin/sleep", Args: []string{"30"},
		MemoryBytes: 64 << 20, CPUMillicores: 500}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(p.PID) + "/cgroup")
	if !strings.Contains(string(b), "/"+unit+"/strategies/slot/leaf") {
		t.Fatalf("payload cgroup = %q", b)
	}
	if got := readTrim(t, filepath.Join(root, "slot/memory.max")); got != strconv.Itoa(64<<20) {
		t.Fatalf("memory.max = %s", got)
	}
	if got := readTrim(t, filepath.Join(root, "slot/cpu.max")); got != "50000 100000" {
		t.Fatalf("cpu.max = %s", got)
	}
	_ = d.Signal(p, syscall.SIGKILL)
	d.WatchExit(p, time.Now)

	// The payload is uid 0 in its user namespace. Unmounting the leaf
	// cover would reveal the host cgroupfs, so the script tries that
	// before writing the slot limit and moving itself up.
	sealScript := fmt.Sprintf(`uid=$(id -u)
if [ "$uid" != 0 ]; then exit 44; fi
slot=/sys/fs/cgroup/%s/strategies/seal
umount /sys/fs/cgroup 2>/dev/null
if echo 1073741824 > "$slot/memory.max" 2>/dev/null; then exit 42; fi
if echo $$ > "$slot/cgroup.procs" 2>/dev/null; then exit 43; fi
exit 0
`, unit)
	p, err = d.Start(StartSpec{Strategy: "seal", BinaryPath: "/bin/sh", Args: []string{"-c", sealScript},
		MemoryBytes: 64 << 20}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if info := d.WatchExit(p, time.Now); info.Code != 0 {
		t.Fatalf("payload escaped its cgroup, exit %d", info.Code)
	}
	if got := readTrim(t, filepath.Join(root, "seal/memory.max")); got != strconv.Itoa(64<<20) {
		t.Fatalf("memory.max after seal attempt = %s", got)
	}

	// Restart without limits lifts them on the same slot.
	p, err = d.Start(StartSpec{Strategy: "slot", BinaryPath: "/bin/sleep", Args: []string{"30"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := readTrim(t, filepath.Join(root, "slot/memory.max")); got != "max" {
		t.Fatalf("memory.max after unset = %s", got)
	}
	_ = d.Signal(p, syscall.SIGKILL)
	d.WatchExit(p, time.Now)

	// A descendant that outgrows the slot is OOM-killed inside it.
	// Swap would let it page out instead; the slot is pre-created to pin it to 0.
	_ = os.MkdirAll(filepath.Join(root, "hog"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "hog/memory.swap.max"), []byte("0"), 0o644)
	p, err = d.Start(StartSpec{Strategy: "hog", BinaryPath: "/bin/sh",
		Args:        []string{"-c", `x=$(head -c 200000000 /dev/zero | tr '\0' a); sleep 30`},
		MemoryBytes: 32 << 20}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Signal(p, syscall.SIGKILL); d.WatchExit(p, time.Now) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, kills, ok := SlotCgroupStats(root, "hog")
		if ok && kills > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no oom_kill in hog slot (ok=%v kills=%d)", ok, kills)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, _, kills, _ := SlotCgroupStats(root, "slot"); kills != 0 {
		t.Fatalf("OOM leaked into another slot: %d", kills)
	}
}

func TestResolveCgroupRootRefusesUnitCgroup(t *testing.T) {
	own, err := ownCgroup()
	if err != nil {
		t.Skip(err)
	}
	_, err = ResolveCgroupRoot(CgroupRootAuto)
	if strings.HasSuffix(own, ".service") || strings.HasSuffix(own, ".scope") || strings.HasSuffix(own, ".slice") {
		if err == nil {
			t.Fatalf("auto must refuse unit cgroup %s", own)
		}
	}
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func nofileLimit(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/limits")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Max open files") {
			f := strings.Fields(strings.TrimPrefix(line, "Max open files"))
			if len(f) >= 2 {
				return f[0] + " " + f[1]
			}
		}
	}
	t.Fatalf("no Max open files in %s", b)
	return ""
}

// waitComm waits until pid has exec'd into comm (the shim replaces itself).
func waitComm(t *testing.T, pid int, comm string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if strings.TrimSpace(string(b)) == comm {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never became %s", pid, comm)
}

// waitChild returns the first child of pid.
func waitChild(t *testing.T, pid int) int {
	t.Helper()
	path := "/proc/" + strconv.Itoa(pid) + "/task/" + strconv.Itoa(pid) + "/children"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(path)
		if f := strings.Fields(string(b)); len(f) > 0 {
			c, _ := strconv.Atoi(f[0])
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d has no child", pid)
	return 0
}

// The OCI child sets the rlimit before --oci-init runs, so the container
// payload (the init pid itself after exec) carries it.
func TestOCIMaxOpenFiles(t *testing.T) {
	requireUserNS(t)
	_, _, sleep, rootfs, work := ociToolRootfs(t)
	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy: "s", Driver: KindOCI, Rootfs: rootfs,
		Argv: []string{sleep, "30"}, WorkDir: work, WorkBind: work,
		Env: []string{"PATH=/bin:/usr/bin"}, MaxOpenFiles: 321,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Signal(p, syscall.SIGKILL); d.WatchExit(p, time.Now) }()
	waitComm(t, p.PID, "sleep")
	if got := nofileLimit(t, p.PID); got != "321 321" {
		t.Fatalf("Max open files = %q, want 321 321", got)
	}
}
