package telemetry

import (
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/driver"
)

func supportedDrivers() []pb.ExecutionDriver {
	out := []pb.ExecutionDriver{pb.ExecutionDriver_EXECUTION_DRIVER_EXEC}
	if driver.UserNSAvailable() {
		out = append(out, pb.ExecutionDriver_EXECUTION_DRIVER_OCI)
	}
	return out
}
