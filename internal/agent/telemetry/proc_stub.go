//go:build !linux

package telemetry

import (
	"fmt"
	"runtime"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

type hostCPUSample struct {
	total uint64
	idle  uint64
}

type procCPUSample struct {
	total uint64
	host  uint64
}

func sampleMachine(_ *hostCPUSample) (*pb.MachineResources, *hostCPUSample, error) {
	return nil, nil, fmt.Errorf("resource sampling is only supported on linux")
}

func sampleProcess(_ int32, prev procCPUSample) (int64, int32, float64, procCPUSample, error) {
	return 0, 0, 0, prev, fmt.Errorf("resource sampling is only supported on linux")
}

// MachineSpecFromHost fills what we can without /proc.
func MachineSpecFromHost() *pb.MachineSpec {
	return &pb.MachineSpec{
		Os:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		NumCpus:          int32(runtime.NumCPU()),
		SupportedDrivers: []pb.ExecutionDriver{pb.ExecutionDriver_EXECUTION_DRIVER_EXEC},
	}
}
