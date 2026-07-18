// Command controlplane runs the Strategon control plane: the AgentService bidi
// stream endpoint over h2c (plaintext HTTP/2 for local/dev; mTLS is a deferred
// follow-up), backed by the in-memory store. A tiny admin endpoint lets you set
// a strategy assignment and watch an agent converge.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	resync := flag.Duration("resync", 30*time.Second, "periodic full-resync interval")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	st := store.NewMemory()
	srv := grpcstream.New(st, grpcstream.WithResync(*resync), grpcstream.WithLogger(logger))

	mux := http.NewServeMux()
	path, handler := strategyplatformv1connect.NewAgentServiceHandler(srv)
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	registerAdmin(mux, st, srv)

	h2s := &http2.Server{}
	httpServer := &http.Server{Addr: *addr, Handler: h2c.NewHandler(mux, h2s)}
	logger.Info("control plane listening", "addr", *addr)
	if err := httpServer.ListenAndServe(); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

// registerAdmin exposes a minimal JSON endpoint for setting an assignment. The
// full human-facing ControlPlaneService (Connect) is a deferred follow-up; this
// stand-in exercises the "change desired state -> agent converges" path.
func registerAdmin(mux *http.ServeMux, st store.Store, srv *grpcstream.Server) {
	mux.HandleFunc("/admin/assign", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			MachineID string `json:"machine_id"`
			Strategy  string `json:"strategy"`
			Version   string `json:"version"`
			Digest    string `json:"digest"`
			URI       string `json:"uri"`
			Remove    bool   `json:"remove"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var spec *pb.StrategyAssignmentSpec
		if !req.Remove {
			spec = &pb.StrategyAssignmentSpec{
				Strategy: req.Strategy,
				Artifact: &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Version: req.Version, Digest: req.Digest, Uri: req.URI},
				DeployPolicy: &pb.DeployPolicy{Startsecs: 2, HealthWindowSeconds: 10, MaxCrashesInWindow: 3, StopGraceSeconds: 5, EnableAutoRollback: true},
			}
		}
		gen, err := st.SetAssignment(req.MachineID, req.Strategy, spec)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		srv.Notify(req.MachineID)
		_ = json.NewEncoder(w).Encode(map[string]int64{"generation": gen})
	})
}
