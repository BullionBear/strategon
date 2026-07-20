// Command agent runs the Strategon agent: the level-triggered reconciler plus
// the outbound stream client. Local/dev uses plaintext h2c; with
// --tls-cert/--tls-key/--server-ca the agent dials HTTPS and presents an
// Ed25519 client certificate (mTLS).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/reconciler"
	"github.com/bullionbear/strategon/internal/agent/stream"
	"github.com/bullionbear/strategon/internal/agent/telemetry"
	"github.com/bullionbear/strategon/internal/buildinfo"
	"github.com/bullionbear/strategon/internal/clock"
	"github.com/bullionbear/strategon/internal/mtls"
	"golang.org/x/net/http2"
)

func main() {
	controlURL := flag.String("control-plane", "http://127.0.0.1:8080", "control plane base URL (http for h2c, https for mTLS)")
	machineID := flag.String("machine-id", "", "machine id (defaults to client cert CN when mTLS is enabled)")
	base := flag.String("base", "/opt/strategies", "strategy release base directory")
	cgroupRoot := flag.String("cgroup-root", "", "delegated cgroup v2 root (empty disables confinement)")
	agentVersion := flag.Int("agent-version", 2, "agent capability version (monotonic)")
	metricsListen := flag.String("metrics-listen", "", "optional Prometheus text /metrics listen address (e.g. 127.0.0.1:9101); empty disables")
	region := flag.String("region", "", "operator-assigned region label for fleet grouping (e.g. tw); empty groups as Unassigned")
	zone := flag.String("zone", "", "operator-assigned zone label within a region")

	tlsCert := flag.String("tls-cert", "", "client certificate PEM (enables mTLS with --tls-key and --server-ca)")
	tlsKey := flag.String("tls-key", "", "client private key PEM")
	serverCA := flag.String("server-ca", "", "CA PEM used to verify the control plane certificate")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	httpClient, idFromCert, err := newHTTPClient(*tlsCert, *tlsKey, *serverCA)
	if err != nil {
		logger.Error("tls config failed", "err", err)
		os.Exit(2)
	}
	if *machineID == "" {
		*machineID = idFromCert
	}
	if *machineID == "" {
		logger.Error("--machine-id is required (or provide an mTLS client cert with CN)")
		os.Exit(2)
	}
	if idFromCert != "" && idFromCert != *machineID {
		logger.Error("machine-id does not match client certificate CN", "machine_id", *machineID, "cert_cn", idFromCert)
		os.Exit(2)
	}
	if strings.HasPrefix(*controlURL, "https://") && *tlsCert == "" {
		logger.Error("https:// control-plane requires --tls-cert/--tls-key/--server-ca")
		os.Exit(2)
	}
	if *tlsCert != "" && strings.HasPrefix(*controlURL, "http://") {
		logger.Error("mTLS requires https:// control-plane URL")
		os.Exit(2)
	}

	hostname, _ := os.Hostname()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	out := make(chan *pb.AgentMessage, 128)
	artifacts := artifact.NewManager(*base, artifact.NewDefaultFetcher())
	rec := reconciler.New(reconciler.Deps{
		Driver:       driver.NewExecDriver(*cgroupRoot),
		Artifacts:    artifacts,
		Health:       health.AlwaysReady{},
		Clock:        clock.Real{},
		Out:          out,
		BaseDir:      *base,
		AgentVersion: *agentVersion,
		Logger:       logger,
	})

	collector := telemetry.New(func() []telemetry.ProcessTarget {
		raw := rec.ProcessTargets()
		out := make([]telemetry.ProcessTarget, len(raw))
		for i, t := range raw {
			out[i] = telemetry.ProcessTarget{
				Strategy:     t.Strategy,
				PID:          t.PID,
				Alive:        t.Alive,
				RestartCount: t.RestartCount,
			}
		}
		return out
	})
	collector.Logger = logger
	go collector.Run(ctx)

	if *metricsListen != "" {
		go func() {
			logger.Info("metrics endpoint listening", "addr", *metricsListen)
			if err := telemetry.ListenAndServeMetrics(*metricsListen, *machineID, collector); err != nil && ctx.Err() == nil {
				logger.Error("metrics server exited", "err", err)
			}
		}()
	}

	client := &stream.Client{
		Register: &pb.Register{
			MachineId:         *machineID,
			Hostname:          hostname,
			AgentVersion:      int32(*agentVersion),
			AgentBuildVersion: buildinfo.Version,
			Spec:              hostSpec(*region, *zone),
		},
		Client:      strategyplatformv1connect.NewAgentServiceClient(httpClient, *controlURL, connect.WithGRPC()),
		Out:         out,
		Submit:      rec.SubmitDesired,
		ObservedGen: rec.ObservedGeneration,
		Artifacts:   artifacts,
		Resources:   collector.HeartbeatResources,
		Processes:   collector.HeartbeatProcesses,
		Clock:       clock.Real{},
		Heartbeat:   5 * time.Second,
		Logger:      logger,
	}

	go rec.Run(ctx)
	logger.Info("agent started", "machine_id", *machineID, "control_plane", *controlURL,
		"mtls", *tlsCert != "", "build_version", buildinfo.Version)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("stream client exited", "err", err)
		os.Exit(1)
	}
}

func newHTTPClient(certFile, keyFile, serverCAFile string) (*http.Client, string, error) {
	tlsMode := certFile != "" || keyFile != "" || serverCAFile != ""
	if !tlsMode {
		return &http.Client{Transport: h2cTransport()}, "", nil
	}
	if certFile == "" || keyFile == "" || serverCAFile == "" {
		return nil, "", fmt.Errorf("mTLS requires --tls-cert, --tls-key, and --server-ca together")
	}
	cert, err := mtls.LoadCert(certFile, keyFile)
	if err != nil {
		return nil, "", err
	}
	cn, err := mtls.CertCN(cert)
	if err != nil {
		return nil, "", err
	}
	serverCA, err := mtls.LoadCAPool(serverCAFile)
	if err != nil {
		return nil, "", err
	}
	return &http.Client{Transport: &http2.Transport{
		TLSClientConfig: mtls.ClientConfig(cert, serverCA),
	}}, cn, nil
}

// h2cTransport dials plaintext HTTP/2 (h2c) so bidi streaming works without TLS
// in local/dev.
func h2cTransport() *http2.Transport {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}

// hostSpec fills the machine spec from the host and overlays the operator's
// placement labels. Region and zone are policy, not something the host can
// discover, so they arrive as flags rather than from /proc.
func hostSpec(region, zone string) *pb.MachineSpec {
	spec := telemetry.MachineSpecFromHost()
	spec.Region = strings.TrimSpace(region)
	spec.Zone = strings.TrimSpace(zone)
	return spec
}
