// Command controlplane runs the Strategon control plane:
//
//   - AgentService on --agent-addr (default :8080) — agent-outbound bidi stream
//   - ControlPlaneService on --human-addr (default 127.0.0.1:8081) — human HTTP/JSON
//
// Separate ports match FRONTEND.md §0: different clients, different mental
// models. The human port binds loopback by default (no auth yet). Agent port
// optionally requires mTLS (--tls-cert/--tls-key/--client-ca).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/buildinfo"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	cpLease "github.com/bullionbear/strategon/internal/controlplane/lease"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	agentAddr := flag.String("agent-addr", ":8080", "AgentService listen address")
	humanAddr := flag.String("human-addr", "127.0.0.1:8081", "ControlPlaneService listen address (bind loopback; no auth)")
	// Legacy alias for agent addr.
	legacyAddr := flag.String("addr", "", "deprecated alias for --agent-addr")
	resync := flag.Duration("resync", 30*time.Second, "periodic full-resync interval for agents")
	dbDSN := flag.String("db", "", "PostgreSQL DSN (e.g. postgres://user:pass@host:5432/strategon); empty = in-memory store")

	tlsCert := flag.String("tls-cert", "", "AgentService TLS certificate (PEM); enables mTLS with --tls-key and --client-ca")
	tlsKey := flag.String("tls-key", "", "AgentService TLS private key (PEM)")
	clientCA := flag.String("client-ca", "", "CA PEM used to verify agent client certificates")
	leaseMarginCP := flag.Duration("lease-margin-cp", store.DefaultLeaseMarginCP, "control-plane lease expiry margin (SAFETY §2)")
	flag.Parse()
	if *legacyAddr != "" {
		*agentAddr = *legacyAddr
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	hub := store.NewHub()
	var st store.Store
	if *dbDSN != "" {
		pg, err := store.NewPostgres(context.Background(), *dbDSN, hub)
		if err != nil {
			logger.Error("postgres store init failed", "err", err)
			os.Exit(1)
		}
		defer pg.Close()
		pg.SetLeaseMarginCP(*leaseMarginCP)
		st = pg
		logger.Info("using postgres store")
	} else {
		mem := store.NewMemory(hub)
		mem.SetLeaseMarginCP(*leaseMarginCP)
		st = mem
		logger.Info("using in-memory store (state lost on restart; set --db for durability)")
	}
	agentSrv := grpcstream.New(st, grpcstream.WithResync(*resync), grpcstream.WithLogger(logger))
	leaseSrv := cpLease.New(st, logger)
	humanSrv := api.New(st, hub, agentSrv, logger)

	agentMux := http.NewServeMux()
	agentPath, agentHandler := strategyplatformv1connect.NewAgentServiceHandler(agentSrv)
	agentMux.Handle(agentPath, agentHandler)
	leasePath, leaseHandler := strategyplatformv1connect.NewLeaseServiceHandler(leaseSrv)
	agentMux.Handle(leasePath, leaseHandler)
	agentMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	humanMux := http.NewServeMux()
	humanPath, humanHandler := strategyplatformv1connect.NewControlPlaneServiceHandler(humanSrv)
	humanMux.Handle(humanPath, withCORS(humanHandler))
	humanMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	h2s := &http2.Server{}
	go func() {
		agentHandler := mtls.PeerCNHandler(agentMux)
		if err := serveAgent(logger, *agentAddr, agentHandler, h2s, *tlsCert, *tlsKey, *clientCA); err != nil {
			logger.Error("agent server exited", "err", err)
			os.Exit(1)
		}
	}()

	logger.Info("human API listening", "addr", *humanAddr,
		"build_version", buildinfo.Version, "commit", buildinfo.CommitHash, "build_time", buildinfo.BuildTime)
	humanHTTP := &http.Server{Addr: *humanAddr, Handler: h2c.NewHandler(humanMux, h2s)}
	if err := humanHTTP.ListenAndServe(); err != nil {
		logger.Error("human server exited", "err", err)
		os.Exit(1)
	}
}

func serveAgent(logger *slog.Logger, addr string, handler http.Handler, h2s *http2.Server, certFile, keyFile, clientCAFile string) error {
	tlsMode := certFile != "" || keyFile != "" || clientCAFile != ""
	if tlsMode {
		if certFile == "" || keyFile == "" || clientCAFile == "" {
			return fmt.Errorf("mTLS requires --tls-cert, --tls-key, and --client-ca together")
		}
		cert, err := mtls.LoadCert(certFile, keyFile)
		if err != nil {
			return err
		}
		clientCA, err := mtls.LoadCAPool(clientCAFile)
		if err != nil {
			return err
		}
		srv := &http.Server{
			Addr:      addr,
			Handler:   handler,
			TLSConfig: mtls.ServerConfig(cert, clientCA),
		}
		if err := http2.ConfigureServer(srv, h2s); err != nil {
			return err
		}
		logger.Info("agent service listening (mTLS)", "addr", addr)
		// Certificates are already in TLSConfig; empty paths are intentional.
		return srv.ListenAndServeTLS("", "")
	}

	logger.Info("agent service listening (h2c)", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: h2c.NewHandler(handler, h2s)}
	return srv.ListenAndServe()
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
