//go:build linux

package telemetry

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

type hostCPUSample struct {
	total uint64
	idle  uint64
}

type procCPUSample struct {
	total uint64 // process utime+stime
	host  uint64 // host total jiffies at same instant
}

func sampleMachine(prev *hostCPUSample) (*pb.MachineResources, *hostCPUSample, error) {
	cur, err := readHostCPU()
	if err != nil {
		return nil, nil, err
	}
	memUsed, memTotal, err := readMemInfo()
	if err != nil {
		return nil, nil, err
	}
	load1, _ := readLoad1()
	diskUsed, diskTotal, _ := readDisk("/")
	netRx, netTx, _ := readNetDev()

	res := &pb.MachineResources{
		MemoryUsedBytes:  memUsed,
		MemoryTotalBytes: memTotal,
		DiskUsedBytes:    diskUsed,
		DiskTotalBytes:   diskTotal,
		Load1:            load1,
		NetRxBytes:       netRx,
		NetTxBytes:       netTx,
	}
	if prev != nil {
		res.CpuPercent = cpuPercent(prev.total, prev.idle, cur.total, cur.idle)
	}
	return res, &cur, nil
}

func sampleProcess(pid int32, prev procCPUSample) (rss int64, fds int32, cpu float64, next procCPUSample, err error) {
	utime, stime, err := readProcStat(pid)
	if err != nil {
		return 0, 0, 0, prev, err
	}
	host, err := readHostCPU()
	if err != nil {
		return 0, 0, 0, prev, err
	}
	rss, fds = readProcStatus(pid)
	next = procCPUSample{total: utime + stime, host: host.total}
	if prev.host > 0 && host.total > prev.host && next.total >= prev.total {
		deltaProc := float64(next.total - prev.total)
		deltaHost := float64(host.total - prev.host)
		if deltaHost > 0 {
			cpu = (deltaProc / deltaHost) * 100 * float64(runtime.NumCPU())
			if cpu < 0 {
				cpu = 0
			}
			if cpu > 100*float64(runtime.NumCPU()) {
				cpu = 100 * float64(runtime.NumCPU())
			}
		}
	}
	return rss, fds, cpu, next, nil
}

func readHostCPU() (hostCPUSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return hostCPUSample{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return hostCPUSample{}, fmt.Errorf("empty /proc/stat")
	}
	fields := strings.Fields(sc.Text())
	// cpu user nice system idle iowait irq softirq steal ...
	if len(fields) < 5 || fields[0] != "cpu" {
		return hostCPUSample{}, fmt.Errorf("unexpected /proc/stat line")
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return hostCPUSample{}, err
		}
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	return hostCPUSample{total: total, idle: idle}, nil
}

func cpuPercent(prevTotal, prevIdle, curTotal, curIdle uint64) float64 {
	dt := curTotal - prevTotal
	di := curIdle - prevIdle
	if dt == 0 || curTotal < prevTotal {
		return 0
	}
	used := float64(dt-di) / float64(dt) * 100
	if used < 0 {
		return 0
	}
	if used > 100 {
		return 100
	}
	return used
}

func readMemInfo() (used, total int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var memTotal, memAvail int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			memTotal = parseMemKB(line) * 1024
		case strings.HasPrefix(line, "MemAvailable:"):
			memAvail = parseMemKB(line) * 1024
		}
		if memTotal > 0 && memAvail > 0 {
			break
		}
	}
	if memTotal <= 0 {
		return 0, 0, fmt.Errorf("MemTotal missing")
	}
	used = memTotal - memAvail
	if used < 0 {
		used = 0
	}
	return used, memTotal, nil
}

func parseMemKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(fields[1], 10, 64)
	return v
}

func readLoad1() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 {
		return 0, fmt.Errorf("empty loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readDisk(path string) (used, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	used = total - free
	if used < 0 {
		used = 0
	}
	return used, total, nil
}

func readNetDev() (rx, tx int64, err error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// skip headers
	sc.Scan()
	sc.Scan()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseInt(fields[0], 10, 64)
		t, _ := strconv.ParseInt(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx, nil
}

func readProcStat(pid int32) (utime, stime uint64, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	// comm is in parentheses and may contain spaces
	rparen := strings.LastIndex(s, ")")
	if rparen < 0 || rparen+2 >= len(s) {
		return 0, 0, fmt.Errorf("bad /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[rparen+2:])
	// after comm: state(1) ... utime is field 12, stime 13 in full numbering
	// fields[0]=state, so utime=fields[11], stime=fields[12]
	if len(fields) < 13 {
		return 0, 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	utime, err = strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	stime, err = strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return utime, stime, nil
}

func readProcStatus(pid int32) (rss int64, fds int32) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			rss = parseMemKB(line) * 1024
		}
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err == nil {
		fds = int32(len(entries))
	}
	return rss, fds
}

// MachineSpecFromHost fills static enrollment metadata from the local host.
func MachineSpecFromHost() *pb.MachineSpec {
	spec := &pb.MachineSpec{
		Os:               "linux",
		Arch:             runtime.GOARCH,
		NumCpus:          int32(runtime.NumCPU()),
		SupportedDrivers: []pb.ExecutionDriver{pb.ExecutionDriver_EXECUTION_DRIVER_EXEC},
	}
	if _, total, err := readMemInfo(); err == nil {
		spec.MemoryTotalBytes = total
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		spec.KernelVersion = strings.TrimSpace(string(b))
	}
	return spec
}
