package driver

import (
	"strings"
	"testing"
)

func TestValidateCPUMillicores(t *testing.T) {
	ok := []int64{0, MinCPUMillicores, 500, MaxCPUMillicores}
	for _, n := range ok {
		if err := ValidateCPUMillicores(n); err != nil {
			t.Fatalf("ValidateCPUMillicores(%d) = %v", n, err)
		}
	}
	if err := ValidateCPUMillicores(5); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("5 millicores: %v", err)
	}
	if err := ValidateCPUMillicores(MinCPUMillicores - 1); err == nil {
		t.Fatal("just below the minimum was accepted")
	}
	huge := int64(100_000_000_000_000) // 1e14
	if err := ValidateCPUMillicores(huge); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("1e14: %v", err)
	}
	if err := ValidateCPUMillicores(MaxCPUMillicores + 1); err == nil {
		t.Fatal("just above the maximum was accepted")
	}
	if err := ValidateCPUMillicores(-1); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative: %v", err)
	}
}

func TestCPUMaxFile(t *testing.T) {
	got, err := cpuMaxFile(0)
	if err != nil || got != "max 100000" {
		t.Fatalf("unset = %q %v", got, err)
	}
	got, err = cpuMaxFile(10)
	if err != nil || got != "1000 100000" {
		t.Fatalf("10 = %q %v", got, err)
	}
	got, err = cpuMaxFile(500)
	if err != nil || got != "50000 100000" {
		t.Fatalf("500 = %q %v", got, err)
	}
	if _, err := cpuMaxFile(5); err == nil {
		t.Fatal("cpu.max formatter accepted 5 millicores")
	}
	if _, err := cpuMaxFile(100_000_000_000_000); err == nil {
		t.Fatal("cpu.max formatter accepted 1e14")
	}
}

func TestCheckLimitsRejectsCPURangeBeforeCgroup(t *testing.T) {
	d := NewExecDriver("")
	err := d.CheckLimits(StartSpec{Strategy: "s", CPUMillicores: 5, MemoryBytes: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("cpu 5 with no cgroup root: %v", err)
	}
	err = d.CheckLimits(StartSpec{Strategy: "s", CPUMillicores: 100_000_000_000_000})
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("cpu 1e14: %v", err)
	}
}
