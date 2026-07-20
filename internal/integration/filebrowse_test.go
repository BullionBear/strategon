// TestWorkDirBrowseAndDownloadEndToEnd spins up control plane + in-process
// agent, browses a temp strategy WorkDir, and downloads single + multi files
// through the human API. Also asserts heartbeats keep updating during a
// moderately large transfer.
package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
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
	"github.com/bullionbear/strategon/internal/agent/stream"
	"github.com/bullionbear/strategon/internal/clock"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/filetransfer"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestWorkDirBrowseAndDownloadEndToEnd(t *testing.T) {
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
	strategy := "alpha"
	work := filepath.Join(base, strategy)
	if err := os.MkdirAll(filepath.Join(work, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "readme.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "logs", "app.log"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Larger payload to exercise chunking during heartbeat window.
	big := bytes.Repeat([]byte("x"), 200*1024)
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	artifacts := artifact.NewManager(base, artifact.LocalFetcher{})
	out := make(chan *pb.AgentMessage, 64)
	httpClient := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	agentClient := &stream.Client{
		Register: &pb.Register{
			MachineId:    "m-browse",
			Hostname:     "test",
			AgentVersion: 2,
		},
		Client:    strategyplatformv1connect.NewAgentServiceClient(httpClient, ts.URL, connect.WithGRPC()),
		Out:       out,
		Submit:    func(*pb.DesiredState) {},
		Artifacts: artifacts,
		Clock:     clock.Real{},
		Heartbeat: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agentClient.Run(ctx)

	waitUntil(t, 5*time.Second, func() bool {
		rec, ok := st.GetMachine("m-browse")
		return ok && rec.Reachable && rec.AgentVersion >= 2
	}, "agent to register")

	// --- Browse root ---
	browse, err := humanClient.BrowseDir(ctx, connect.NewRequest(&pb.BrowseDirRequest{
		MachineId: "m-browse",
		Strategy:  strategy,
		Path:      ".",
	}))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range browse.Msg.GetEntries() {
		names[e.GetName()] = e.GetIsDir()
	}
	if _, ok := names["readme.txt"]; !ok {
		t.Fatalf("missing readme.txt in %v", names)
	}
	if !names["logs"] {
		t.Fatalf("logs should be a directory: %v", names)
	}
	if _, ok := names["big.bin"]; !ok {
		t.Fatalf("missing big.bin in %v", names)
	}

	// --- Single-file download ---
	single, err := humanClient.DownloadFiles(ctx, connect.NewRequest(&pb.DownloadFilesRequest{
		MachineId: "m-browse",
		Strategy:  strategy,
		Paths:     []string{"readme.txt"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var singleBuf bytes.Buffer
	var singleName string
	for single.Receive() {
		chunk := single.Msg()
		if chunk.GetFilename() != "" {
			singleName = chunk.GetFilename()
		}
		singleBuf.Write(chunk.GetData())
		if chunk.GetEof() {
			break
		}
	}
	if err := single.Err(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if singleName != "readme.txt" || singleBuf.String() != "hello" {
		t.Fatalf("single name=%q data=%q", singleName, singleBuf.String())
	}

	// --- Multi download (tarball) while heartbeats continue ---
	hbBefore, _ := st.GetMachine("m-browse")
	lastHB := hbBefore.LastHeartbeat

	multi, err := humanClient.DownloadFiles(ctx, connect.NewRequest(&pb.DownloadFilesRequest{
		MachineId: "m-browse",
		Strategy:  strategy,
		Paths:     []string{"readme.txt", "big.bin", "logs"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var multiBuf bytes.Buffer
	var multiName string
	var kind pb.TransferKind
	for multi.Receive() {
		chunk := multi.Msg()
		if chunk.GetFilename() != "" {
			multiName = chunk.GetFilename()
		}
		if chunk.GetTransferKind() != pb.TransferKind_TRANSFER_KIND_UNSPECIFIED {
			kind = chunk.GetTransferKind()
		}
		multiBuf.Write(chunk.GetData())
		if chunk.GetEof() {
			break
		}
	}
	if err := multi.Err(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if kind != pb.TransferKind_TRANSFER_KIND_TARBALL {
		t.Fatalf("kind=%v", kind)
	}
	if multiName == "" || multiBuf.Len() < 100 {
		t.Fatalf("tarball name=%q len=%d", multiName, multiBuf.Len())
	}
	if multiBuf.Bytes()[0] != 0x1f || multiBuf.Bytes()[1] != 0x8b {
		t.Fatal("expected gzip magic")
	}

	waitUntil(t, 3*time.Second, func() bool {
		rec, ok := st.GetMachine("m-browse")
		return ok && rec.LastHeartbeat > lastHB
	}, "heartbeat during/after transfer")

	audits := st.ListAudit("m-browse", strategy)
	found := false
	for _, a := range audits {
		if a.GetAction() == "DownloadFiles" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected DownloadFiles audit entry")
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
