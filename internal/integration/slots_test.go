package integration

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/reconciler"
	"github.com/bullionbear/strategon/internal/agent/stream"
	"github.com/bullionbear/strategon/internal/clock"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/filetransfer"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestStrategySlotsListAndReap(t *testing.T) {
	hub := store.NewHub()
	st := store.NewMemory(hub)
	broker := filetransfer.New()
	agentSrv := grpcstream.New(st, grpcstream.WithResync(time.Hour), grpcstream.WithBroker(broker))
	humanSrv := api.NewWithBroker(st, hub, agentSrv, broker, nil)

	mux := http.NewServeMux()
	agentPath, agentHandler := strategyplatformv1connect.NewAgentServiceHandler(agentSrv)
	mux.Handle(agentPath, agentHandler)
	humanPath, humanHandler := strategyplatformv1connect.NewControlPlaneServiceHandler(humanSrv)
	mux.Handle(humanPath, humanHandler)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	defer ts.Close()

	humanClient := strategyplatformv1connect.NewControlPlaneServiceClient(http.DefaultClient, ts.URL)

	base := t.TempDir()
	slot := filepath.Join(base, "probe-fail")
	if err := os.MkdirAll(filepath.Join(slot, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "work", "leftover.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	artifacts := artifact.NewManager(base, artifact.LocalFetcher{})
	out := make(chan *pb.AgentMessage, 256)
	rec := reconciler.New(reconciler.Deps{
		Driver:       driver.NewExecDriver(""),
		Artifacts:    artifacts,
		Health:       health.AlwaysReady{},
		Clock:        clock.Real{},
		Out:          out,
		TickInterval: 100 * time.Millisecond,
	})
	httpClient := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	agentClient := &stream.Client{
		Register: &pb.Register{
			MachineId:    "m-slots",
			Hostname:     "test",
			AgentVersion: 5,
		},
		Client:      strategyplatformv1connect.NewAgentServiceClient(httpClient, ts.URL, connect.WithGRPC()),
		Out:         out,
		Submit:      rec.SubmitDesired,
		Reap:        rec.SubmitReap,
		ObservedGen: rec.ObservedGeneration,
		Artifacts:   artifacts,
		Clock:       clock.Real{},
		Heartbeat:   100 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)
	go agentClient.Run(ctx)

	waitUntil(t, 5*time.Second, func() bool {
		rec, ok := st.GetMachine("m-slots")
		return ok && rec.Reachable && rec.AgentVersion >= 5
	}, "agent to register")

	waitUntil(t, 5*time.Second, func() bool {
		rec, ok := st.GetMachine("m-slots")
		if !ok || rec.SlotsStatus == nil {
			return false
		}
		for _, s := range rec.SlotsStatus.GetSlots() {
			if s.GetStrategy() == "probe-fail" {
				return true
			}
		}
		return false
	}, "slot inventory")

	listed, err := humanClient.ListStrategySlots(ctx, connect.NewRequest(&pb.ListStrategySlotsRequest{MachineId: "m-slots"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetSlots()) != 1 || listed.Msg.GetSlots()[0].GetAssigned() {
		t.Fatalf("list = %+v", listed.Msg)
	}

	reap, err := humanClient.ReapStrategies(ctx, connect.NewRequest(&pb.ReapStrategiesRequest{
		MachineId:  "m-slots",
		Strategies: []string{"probe-fail"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(reap.Msg.GetResults()) != 1 || !reap.Msg.GetResults()[0].GetRemoved() {
		t.Fatalf("reap = %+v", reap.Msg)
	}
	if _, err := os.Stat(slot); !os.IsNotExist(err) {
		t.Fatalf("slot still on disk: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		listed, err := humanClient.ListStrategySlots(ctx, connect.NewRequest(&pb.ListStrategySlotsRequest{MachineId: "m-slots"}))
		return err == nil && len(listed.Msg.GetSlots()) == 0
	}, "empty slot list after reap")

	audits := st.ListAudit("m-slots", "")
	found := false
	for _, a := range audits {
		if a.GetAction() == "ReapStrategies" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ReapStrategies audit, got %+v", audits)
	}
}
