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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/buildinfo"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/filetransfer"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	cpLease "github.com/bullionbear/strategon/internal/controlplane/lease"
	"github.com/bullionbear/strategon/internal/controlplane/objectstore"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
	"github.com/bullionbear/strategon/internal/webassets"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const shutdownTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("control plane exited", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
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

	s3Endpoint := flag.String("s3-endpoint", "", "S3-compatible endpoint for artifact store (e.g. http://127.0.0.1:8333); empty disables ResolveArtifactSource")
	s3Bucket := flag.String("s3-bucket", "", "default S3 bucket for artifact ingest (optional for ST-1 manual PutObject)")
	s3Region := flag.String("s3-region", "us-east-1", "S3 region (SeaweedFS accepts any value)")
	s3AccessKey := flag.String("s3-access-key", "", "S3 access key (default: $STRATEGON_S3_ACCESS_KEY)")
	s3SecretKey := flag.String("s3-secret-key", "", "S3 secret key (default: $STRATEGON_S3_SECRET_KEY); prefer the env var so the secret is not visible in process listings")

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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub := store.NewHub()
	var st store.Store
	var pg *store.Postgres
	if *dbDSN != "" {
		var err error
		pg, err = store.NewPostgres(ctx, *dbDSN, hub)
		if err != nil {
			return fmt.Errorf("postgres store init: %w", err)
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

	mode, err := auth.ParseMode(*authMode)
	if err != nil {
		return fmt.Errorf("invalid --auth-mode: %w", err)
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
		Tokens:              st,
		Logger:              logger,
	})
	if err != nil {
		return fmt.Errorf("auth init: %w", err)
	}
	if err := authSvc.LoadTokens(ctx); err != nil {
		return fmt.Errorf("load api tokens: %w", err)
	}
	go authSvc.RunTokenFlusher(ctx, auth.DefaultTokenFlushInterval)

	broker := filetransfer.New()
	agentOpts := []grpcstream.Option{
		grpcstream.WithResync(*resync),
		grpcstream.WithLogger(logger),
		grpcstream.WithBroker(broker),
	}
	s3AK := firstNonEmpty(*s3AccessKey, os.Getenv("STRATEGON_S3_ACCESS_KEY"))
	s3SK := firstNonEmpty(*s3SecretKey, os.Getenv("STRATEGON_S3_SECRET_KEY"))
	if *s3Endpoint != "" || s3AK != "" || s3SK != "" {
		objs, err := objectstore.New(objectstore.Config{
			Endpoint:  *s3Endpoint,
			Bucket:    *s3Bucket,
			Region:    *s3Region,
			AccessKey: s3AK,
			SecretKey: s3SK,
		})
		if err != nil {
			return fmt.Errorf("s3 object store: %w", err)
		}
		agentOpts = append(agentOpts, grpcstream.WithObjectStore(objs))
		logger.Info("s3 object store enabled", "endpoint", *s3Endpoint, "bucket", *s3Bucket, "region", *s3Region)
	}
	agentSrv := grpcstream.New(st, agentOpts...)
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
	agentHTTP, err := newAgentServer(*agentAddr, mtls.PeerCNHandler(agentMux), h2s, *tlsCert, *tlsKey, *clientCA)
	if err != nil {
		return err
	}
	humanHTTP := &http.Server{Addr: *humanAddr, Handler: h2c.NewHandler(withCORS(humanMux), h2s)}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("agent service listening", "addr", *agentAddr, "mtls", *tlsCert != "")
		if err := listenAgent(agentHTTP, *tlsCert != ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("agent server: %w", err)
		}
	}()
	go func() {
		logger.Info("human API listening", "addr", *humanAddr, "auth_mode", mode, "ui_embedded", webassets.Available(),
			"build_version", buildinfo.Version, "commit", buildinfo.CommitHash, "build_time", buildinfo.BuildTime)
		if err := humanHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("human server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		stop()
		// Tear both servers down before returning so deferred pg.Close() does
		// not race with a sibling that is still serving.
		drainServers(logger, humanHTTP, agentHTTP)
		return err
	}

	// Flush last_used on its own budget before drains — streaming connections
	// (agent bidi, WatchMachine) can consume the full shutdownTimeout.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := authSvc.FlushTokens(flushCtx); err != nil {
		logger.Warn("api token flush", "err", err)
	}
	flushCancel()

	drainServers(logger, humanHTTP, agentHTTP)
	return nil
}

// drainServers stops the human API first (short requests + WatchMachine), then
// the agent port. Agent streams rarely idle, so Shutdown is followed by Close.
func drainServers(logger *slog.Logger, humanHTTP, agentHTTP *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := humanHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Warn("human server shutdown", "err", err)
	}
	if err := agentHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Warn("agent server shutdown; forcing close", "err", err)
		_ = agentHTTP.Close()
	}
}

func newAgentServer(addr string, handler http.Handler, h2s *http2.Server, certFile, keyFile, clientCAFile string) (*http.Server, error) {
	tlsMode := certFile != "" || keyFile != "" || clientCAFile != ""
	if tlsMode {
		if certFile == "" || keyFile == "" || clientCAFile == "" {
			return nil, fmt.Errorf("mTLS requires --tls-cert, --tls-key, and --client-ca together")
		}
		cert, err := mtls.LoadCert(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		clientCA, err := mtls.LoadCAPool(clientCAFile)
		if err != nil {
			return nil, err
		}
		srv := &http.Server{
			Addr:      addr,
			Handler:   handler,
			TLSConfig: mtls.ServerConfig(cert, clientCA),
		}
		if err := http2.ConfigureServer(srv, h2s); err != nil {
			return nil, err
		}
		return srv, nil
	}
	return &http.Server{Addr: addr, Handler: h2c.NewHandler(handler, h2s)}, nil
}

func listenAgent(srv *http.Server, mtlsEnabled bool) error {
	if mtlsEnabled {
		// Certificates are already in TLSConfig; empty paths are intentional.
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
