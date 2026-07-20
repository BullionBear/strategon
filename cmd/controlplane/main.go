// Command controlplane runs the Strategon control plane:
//
//   - AgentService on --agent-addr (default :8080) — agent-outbound bidi stream
//   - ControlPlaneService on --human-addr (default 127.0.0.1:8081) — human HTTP/JSON
//
// Separate ports for different clients and different mental
// models. The human port optionally requires Discord/mock auth (--auth-mode).
// Agent port optionally requires mTLS (--tls-cert/--tls-key/--client-ca).
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
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/buildinfo"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/filetransfer"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	cpLease "github.com/bullionbear/strategon/internal/controlplane/lease"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
	"github.com/bullionbear/strategon/internal/webassets"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	agentAddr := flag.String("agent-addr", ":8080", "AgentService listen address")
	humanAddr := flag.String("human-addr", "127.0.0.1:8081", "ControlPlaneService listen address (bind loopback by default)")
	// Legacy alias for agent addr.
	legacyAddr := flag.String("addr", "", "deprecated alias for --agent-addr")
	resync := flag.Duration("resync", 30*time.Second, "periodic full-resync interval for agents")
	dbDSN := flag.String("db", "", "PostgreSQL DSN (e.g. postgres://user:pass@host:5432/strategon); empty = in-memory store")

	tlsCert := flag.String("tls-cert", "", "AgentService TLS certificate (PEM); enables mTLS with --tls-key and --client-ca")
	tlsKey := flag.String("tls-key", "", "AgentService TLS private key (PEM)")
	clientCA := flag.String("client-ca", "", "CA PEM used to verify agent client certificates")
	leaseMarginCP := flag.Duration("lease-margin-cp", store.DefaultLeaseMarginCP, "control-plane lease expiry margin")

	authMode := flag.String("auth-mode", "none", "human API auth: none|mock|discord (default none for local/CI)")
	sessionSecret := flag.String("auth-session-secret", "", "HMAC secret for session cookies; random if empty")
	mockUser := flag.String("auth-mock-user", "local", "username injected in auth-mode=none / mock-login")
	mockID := flag.String("auth-mock-id", "local", "user id injected in auth-mode=none / mock-login")
	discordClientID := flag.String("discord-client-id", "", "Discord OAuth client id (auth-mode=discord)")
	discordClientSecret := flag.String("discord-client-secret", "", "Discord OAuth client secret (auth-mode=discord)")
	discordRedirect := flag.String("discord-redirect-url", "http://127.0.0.1:8081/auth/callback", "Discord OAuth redirect URL")
	discordGuildID := flag.String("discord-guild-id", "", "restrict login to members of this Discord guild (empty = any Discord account)")
	frontendURL := flag.String("auth-frontend-url", "http://127.0.0.1:5173", "browser redirect after login/logout")

	flag.Parse()
	if *legacyAddr != "" {
		*agentAddr = *legacyAddr
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mode, err := auth.ParseMode(*authMode)
	if err != nil {
		logger.Error("invalid --auth-mode", "err", err)
		os.Exit(1)
	}
	authSvc, err := auth.New(auth.Config{
		Mode:                mode,
		SessionSecret:       *sessionSecret,
		MockUser:            *mockUser,
		MockID:              *mockID,
		DiscordClientID:     *discordClientID,
		DiscordClientSecret: *discordClientSecret,
		DiscordRedirectURL:  *discordRedirect,
		DiscordGuildID:      *discordGuildID,
		FrontendURL:         *frontendURL,
		Logger:              logger,
	})
	if err != nil {
		logger.Error("auth init failed", "err", err)
		os.Exit(1)
	}

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
	broker := filetransfer.New()
	agentSrv := grpcstream.New(st, grpcstream.WithResync(*resync), grpcstream.WithLogger(logger), grpcstream.WithBroker(broker))
	leaseSrv := cpLease.New(st, logger)
	humanSrv := api.NewWithBroker(st, hub, agentSrv, broker, logger)

	agentMux := http.NewServeMux()
	agentPath, agentHandler := strategyplatformv1connect.NewAgentServiceHandler(agentSrv)
	agentMux.Handle(agentPath, agentHandler)
	leasePath, leaseHandler := strategyplatformv1connect.NewLeaseServiceHandler(leaseSrv)
	agentMux.Handle(leasePath, leaseHandler)
	agentMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	humanMux := http.NewServeMux()
	humanPath, humanHandler := strategyplatformv1connect.NewControlPlaneServiceHandler(
		humanSrv,
		authSvc.HandlerOptions()...,
	)
	humanMux.Handle(humanPath, humanHandler)
	authSvc.Mount(humanMux)
	humanMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// Catch-all, so the embedded SPA serves every path the routes above don't
	// claim. ServeMux prefers the longest matching pattern, so the RPC, auth
	// and health routes still win over "/".
	humanMux.Handle("/", webassets.Handler())

	h2s := &http2.Server{}
	go func() {
		agentHandler := mtls.PeerCNHandler(agentMux)
		if err := serveAgent(logger, *agentAddr, agentHandler, h2s, *tlsCert, *tlsKey, *clientCA); err != nil {
			logger.Error("agent server exited", "err", err)
			os.Exit(1)
		}
	}()

	logger.Info("human API listening", "addr", *humanAddr, "auth_mode", mode, "ui_embedded", webassets.Available(),
		"build_version", buildinfo.Version, "commit", buildinfo.CommitHash, "build_time", buildinfo.BuildTime)
	humanHTTP := &http.Server{Addr: *humanAddr, Handler: h2c.NewHandler(withCORS(humanMux), h2s)}
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

// withCORS allows the local SvelteKit dev server to call the human API
// (including credentialed fetches and Bearer tokens).
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		if origin != "*" {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent, X-Requested-With")
		w.Header().Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Content-Encoding")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
