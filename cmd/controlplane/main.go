// Command controlplane runs the Strategon control plane:
//
//   - AgentService on --agent-addr (default :8080) — agent-outbound bidi stream
//   - ControlPlaneService on --human-addr (default 127.0.0.1:8081) — human HTTP/JSON
//
// Separate ports match FRONTEND.md §0: different clients, different mental
// models. The human port binds loopback by default (no auth yet).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	agentAddr := flag.String("agent-addr", ":8080", "AgentService listen address")
	humanAddr := flag.String("human-addr", "127.0.0.1:8081", "ControlPlaneService listen address (bind loopback; no auth)")
	// Legacy alias for agent addr.
	legacyAddr := flag.String("addr", "", "deprecated alias for --agent-addr")
	resync := flag.Duration("resync", 30*time.Second, "periodic full-resync interval for agents")
	flag.Parse()
	if *legacyAddr != "" {
		*agentAddr = *legacyAddr
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	hub := store.NewHub()
	st := store.NewMemory(hub)
	agentSrv := grpcstream.New(st, grpcstream.WithResync(*resync), grpcstream.WithLogger(logger))
	humanSrv := api.New(st, hub, agentSrv, logger)

	agentMux := http.NewServeMux()
	agentPath, agentHandler := strategyplatformv1connect.NewAgentServiceHandler(agentSrv)
	agentMux.Handle(agentPath, agentHandler)
	agentMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	humanMux := http.NewServeMux()
	humanPath, humanHandler := strategyplatformv1connect.NewControlPlaneServiceHandler(humanSrv)
	humanMux.Handle(humanPath, withCORS(humanHandler))
	humanMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	h2s := &http2.Server{}
	go func() {
		logger.Info("agent service listening", "addr", *agentAddr)
		srv := &http.Server{Addr: *agentAddr, Handler: h2c.NewHandler(agentMux, h2s)}
		if err := srv.ListenAndServe(); err != nil {
			logger.Error("agent server exited", "err", err)
			os.Exit(1)
		}
	}()

	logger.Info("human API listening", "addr", *humanAddr)
	humanHTTP := &http.Server{Addr: *humanAddr, Handler: h2c.NewHandler(humanMux, h2s)}
	if err := humanHTTP.ListenAndServe(); err != nil {
		logger.Error("human server exited", "err", err)
		os.Exit(1)
	}
}

// withCORS allows the local SvelteKit dev server to call the human API.
// Production sits behind VPN/SSO; this is a local-dev convenience only.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent")
		w.Header().Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Content-Encoding")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
