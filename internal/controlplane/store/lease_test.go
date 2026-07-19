package store

import (
	"strings"
	"testing"
	"time"
)

func TestAcquireRenewDenyAndExpiryMargin(t *testing.T) {
	s := NewMemory(nil)
	s.SetLeaseMarginCP(2 * time.Second)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	now := base
	s.SetClock(func() time.Time { return now })

	r, err := s.AcquireLease("m1", "s", 10*time.Second)
	if err != nil || !r.Granted || r.LeaseID == "" {
		t.Fatalf("acquire m1: %+v err=%v", r, err)
	}
	leaseID := r.LeaseID

	denied, err := s.AcquireLease("m2", "s", 10*time.Second)
	if err != nil || denied.Granted {
		t.Fatalf("m2 should be denied: %+v err=%v", denied, err)
	}
	if !strings.Contains(denied.DenyReason, "held by machine m1") {
		t.Fatalf("deny reason: %q", denied.DenyReason)
	}

	renewed, err := s.RenewLease("m1", "s", leaseID, 10*time.Second)
	if err != nil || !renewed.Granted {
		t.Fatalf("renew m1: %+v err=%v", renewed, err)
	}

	bad, err := s.RenewLease("m2", "s", leaseID, 10*time.Second)
	if err != nil || bad.Granted {
		t.Fatalf("renew by m2 should fail: %+v err=%v", bad, err)
	}

	// Past expires_at but within margin_cp: still blocked for m2.
	now = base.Add(11 * time.Second)
	still, err := s.AcquireLease("m2", "s", 10*time.Second)
	if err != nil || still.Granted {
		t.Fatalf("within margin_cp m2 should still be denied: %+v", still)
	}

	// After expires_at + margin_cp: m2 can acquire.
	now = base.Add(11*time.Second + 2*time.Second + time.Millisecond)
	ok, err := s.AcquireLease("m2", "s", 10*time.Second)
	if err != nil || !ok.Granted {
		t.Fatalf("after margin m2 should acquire: %+v err=%v", ok, err)
	}

	info, found := s.GetLease("s")
	if !found || info.MachineID != "m2" {
		t.Fatalf("GetLease: %+v found=%v", info, found)
	}
}

func TestSameHolderReacquire(t *testing.T) {
	s := NewMemory(nil)
	r1, err := s.AcquireLease("m1", "s", time.Minute)
	if err != nil || !r1.Granted {
		t.Fatal(r1, err)
	}
	r2, err := s.AcquireLease("m1", "s", time.Minute)
	if err != nil || !r2.Granted {
		t.Fatal(r2, err)
	}
	if r2.LeaseID == r1.LeaseID {
		t.Fatalf("reacquire should issue a new lease_id")
	}
}

func TestRenewAfterHardExpiry(t *testing.T) {
	s := NewMemory(nil)
	s.SetLeaseMarginCP(0)
	base := time.Now()
	now := base
	s.SetClock(func() time.Time { return now })

	r, err := s.AcquireLease("m1", "s", time.Second)
	if err != nil || !r.Granted {
		t.Fatal(r, err)
	}
	now = base.Add(2 * time.Second)
	out, err := s.RenewLease("m1", "s", r.LeaseID, time.Second)
	if err != nil || out.Granted {
		t.Fatalf("renew after expiry: %+v err=%v", out, err)
	}
	if out.DenyReason != "lease expired" {
		t.Fatalf("reason=%q", out.DenyReason)
	}
}
