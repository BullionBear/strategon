package main

import (
	"context"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
)

const defaultAddr = "http://127.0.0.1:8081"

type cliConfig struct {
	Addr  string
	Token string
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func normalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = defaultAddr
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

func newClient(cfg cliConfig) strategyplatformv1connect.ControlPlaneServiceClient {
	opts := []connect.ClientOption{}
	if tok := strings.TrimSpace(cfg.Token); tok != "" {
		opts = append(opts, connect.WithInterceptors(bearerInterceptor(tok)))
	}
	return strategyplatformv1connect.NewControlPlaneServiceClient(http.DefaultClient, normalizeAddr(cfg.Addr), opts...)
}

func bearerInterceptor(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
