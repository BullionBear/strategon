package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// DefaultLeaseMarginCP is the control-plane side of the lease expiry margin
// (margin_agent + margin_cp > skew + RTT). Tunable via --lease-margin-cp.
const DefaultLeaseMarginCP = 2 * time.Second

// LeaseInfo is the control plane's view of the fencing lease for one strategy.
type LeaseInfo struct {
	Strategy  string
	MachineID string
	LeaseID   string
	ExpiresAt time.Time
	TTL       time.Duration // last granted TTL; reused on Renew
}

// LeaseResult is the outcome of AcquireLease / RenewLease.
type LeaseResult struct {
	Granted    bool
	LeaseID    string
	ExpiresAt  time.Time
	DenyReason string
}

func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func denyHeld(holder string, until time.Time) string {
	return fmt.Sprintf("held by machine %s until %s", holder, until.UTC().Format(time.RFC3339))
}

// DeploymentBlockedByLease reports whether assigning strategy to machineID is
// forbidden because another machine holds an unexpired lease (+ margin_cp).
func DeploymentBlockedByLease(st Store, machineID, strategy string) (blocked bool, reason string) {
	info, ok := st.GetLease(strategy)
	if !ok {
		return false, ""
	}
	heldUntil := info.ExpiresAt.Add(st.LeaseMarginCP())
	if time.Now().After(heldUntil) {
		return false, ""
	}
	if info.MachineID == machineID {
		return false, ""
	}
	return true, denyHeld(info.MachineID, heldUntil)
}

// leaseFreeFor reports whether machineID may acquire strategy's lease at now,
// given marginCP (another holder blocks until expiresAt+marginCP).
func leaseFreeFor(info *LeaseInfo, machineID string, now time.Time, marginCP time.Duration) (free bool, deny string) {
	if info == nil {
		return true, ""
	}
	heldUntil := info.ExpiresAt.Add(marginCP)
	if now.After(heldUntil) {
		return true, ""
	}
	if info.MachineID == machineID {
		return true, ""
	}
	return false, denyHeld(info.MachineID, heldUntil)
}
