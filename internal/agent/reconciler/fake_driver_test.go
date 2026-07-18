package reconciler

import (
	"errors"
	"sync"
	"syscall"
	"time"

	"github.com/bullionbear/strategon/internal/agent/driver"
)

// fakeDriver is a deterministic driver.Driver for reconciler tests. Processes
// are virtual: Start hands out incrementing pids (startTime == pid), and a
// process stays "alive" until the test calls kill().
type fakeDriver struct {
	mu         sync.Mutex
	nextPID    int
	startCount int
	started    []driver.StartSpec
	signals    []syscall.Signal
	alive      map[int]bool
	exitCh     map[int]chan struct{}
	failStart  bool
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{alive: map[int]bool{}, exitCh: map[int]chan struct{}{}}
}

func (f *fakeDriver) Start(spec driver.StartSpec, now time.Time) (*driver.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return nil, errors.New("fake start failure")
	}
	f.startCount++
	f.nextPID++
	pid := f.nextPID
	f.started = append(f.started, spec)
	f.alive[pid] = true
	f.exitCh[pid] = make(chan struct{})
	return &driver.Process{PID: pid, StartTime: uint64(pid), PGID: pid, StartedAt: now}, nil
}

func (f *fakeDriver) WatchExit(p *driver.Process, now func() time.Time) driver.ExitInfo {
	f.mu.Lock()
	ch := f.exitCh[p.PID]
	f.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return driver.ExitInfo{PID: p.PID, StartTime: p.StartTime, At: now()}
}

func (f *fakeDriver) Signal(p *driver.Process, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return nil
}

func (f *fakeDriver) Adopt(pid int, startTime uint64, startedAt time.Time) (*driver.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.alive[pid] {
		return nil, errors.New("not alive")
	}
	if startTime != 0 && startTime != uint64(pid) {
		return nil, errors.New("starttime mismatch")
	}
	return &driver.Process{PID: pid, StartTime: uint64(pid), PGID: pid, StartedAt: startedAt}, nil
}

// kill marks a process dead and unblocks its WatchExit.
func (f *fakeDriver) kill(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alive[pid] {
		f.alive[pid] = false
		if ch := f.exitCh[pid]; ch != nil {
			close(ch)
			f.exitCh[pid] = nil
		}
	}
}

// closeAll unblocks any pending WatchExit goroutines at test cleanup.
func (f *fakeDriver) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for pid, ch := range f.exitCh {
		if ch != nil {
			close(ch)
			f.exitCh[pid] = nil
		}
		f.alive[pid] = false
	}
}

func (f *fakeDriver) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCount
}
