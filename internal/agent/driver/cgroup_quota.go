package driver

import "fmt"

// cpuPeriodUS is the CFS period written to cpu.max. The kernel's minimum
// quota is 1ms, so the smallest nonzero millicore value is 10
// (quota_us = millicores * period_us / 1000).
//
// The kernel rejects a quota whose nanosecond value exceeds MAX_BW
// ((1<<44)-1 when BW_SHIFT is 20, a little over four hours). quota_ns =
// millicores * period_us, so the largest accepted millicore value is
// MAX_BW / period_us.
const (
	cpuPeriodUS      int64 = 100_000
	MinCPUMillicores int64 = 10
	MaxCPUMillicores int64 = ((1 << 44) - 1) / cpuPeriodUS
)

// ValidateCPUMillicores accepts 0 (unset) and the range the kernel will
// write into cpu.max. Negative values, a quota below 1ms, and a quota
// past MAX_BW are rejected before any cgroup file is touched. The check
// is pure so the control plane and the agent share it.
func ValidateCPUMillicores(n int64) error {
	if n == 0 {
		return nil
	}
	if n < 0 {
		return fmt.Errorf("cpu_millicores %d must not be negative", n)
	}
	if n < MinCPUMillicores {
		return fmt.Errorf("cpu_millicores %d is below the kernel minimum %d (1ms CFS quota over a 100ms period)", n, MinCPUMillicores)
	}
	if n > MaxCPUMillicores {
		return fmt.Errorf("cpu_millicores %d exceeds the kernel maximum %d", n, MaxCPUMillicores)
	}
	return nil
}

// cpuMaxFile is the cpu.max body for millicores. Zero writes "max" so a
// limit removed from the spec is lifted. The quota multiply happens only
// after ValidateCPUMillicores, so a huge millicore value cannot overflow.
func cpuMaxFile(millicores int64) (string, error) {
	if millicores == 0 {
		return fmt.Sprintf("max %d", cpuPeriodUS), nil
	}
	if err := ValidateCPUMillicores(millicores); err != nil {
		return "", err
	}
	quota := millicores * (cpuPeriodUS / 1000)
	return fmt.Sprintf("%d %d", quota, cpuPeriodUS), nil
}
