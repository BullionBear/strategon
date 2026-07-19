// Command agent runs the Strategon agent: the level-triggered reconciler plus
// the outbound stream client. It connects to the control plane over h2c
// (plaintext HTTP/2 for local/dev; mTLS enrollment is a deferred follow-up),
// registers, and converges local state to the pushed DesiredState.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/reconciler"
	"github.com/bullionbear/strategon/internal/agent/stream"
	"github.com/bullionbear/strategon/internal/clock"
	"connectrpc.com/connect"
	"golang.org/x/net/http2"
)

func main() {
	controlURL := flag.String("control-plane", "http://127.0.0.1:8080", "control plane base URL")
	machineID := flag.String("machine-id", "", "machine id (required)")
	base := flag.String("base", "/opt/strategies", "strategy release base directory")
	cgroupRoot := flag.String("cgroup-root", "", "delegated cgroup v2 root (empty disables confinement)")
	agentVersion := flag.Int("agent-version", 1, "agent version")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *machineID == "" {
		logger.Error("--machine-id is required")
		os.Exit(2)
	}

	hostname, _ := os.Hostname()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	out := make(chan *pb.AgentMessage, 128)
	rec := reconciler.New(reconciler.Deps{
		Driver:    driver.NewExecDriver(*cgroupRoot),
		Artifacts: artifact.NewManager(*base, artifact.NewDefaultFetcher()),
		Health:    health.AlwaysReady{},
		Clock:     clock.Real{},
		Out:       out,
	})

	httpClient := &http.Client{Transport: h2cTransport()}
	client := &stream.Client{
		Register: &pb.Register{
			MachineId:    *machineID,
			Hostname:     hostname,
			AgentVersion: int32(*agentVersion),
			Spec:         &pb.MachineSpec{Os: "linux", SupportedDrivers: []pb.ExecutionDriver{pb.ExecutionDriver_EXECUTION_DRIVER_EXEC}},
		},
		Client:      strategyplatformv1connect.NewAgentServiceClient(httpClient, *controlURL, connect.WithGRPC()),
		Out:         out,
		Submit:      rec.SubmitDesired,
		ObservedGen: rec.ObservedGeneration,
		Clock:       clock.Real{},
		Heartbeat:   5 * time.Second,
		Logger:      logger,
	}

	go rec.Run(ctx)
	logger.Info("agent started", "machine_id", *machineID, "control_plane", *controlURL)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("stream client exited", "err", err)
		os.Exit(1)
	}
}

// h2cTransport dials plaintext HTTP/2 (h2c) so bidi streaming works without TLS
// in local/dev. Production would use TLS/mTLS (deferred).
func h2cTransport() *http2.Transport {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}
