// Package health implements the three-layer health model:
//
//	Live            – process is alive (pidfd); owned by the reconciler.
//	Ready           – strategy has connected market data/exchange, queues not
//	                  backed up; probed via a local unix-socket endpoint.
//	BusinessHealthy – business metrics (last tick time, open orders, PnL);
//	                  reported by the strategy's own heartbeat.
//
// The reconciler drives probing on its unified tick and folds results into the
// per-strategy condition list. The Checker is injectable so tests use a fake.
package health

import (
	"context"
	"net"
	"net/http"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// Result is the outcome of a readiness probe.
type Result struct {
	Status  pb.ConditionStatus
	Reason  string
	Message string
}

// Checker probes a strategy's readiness endpoint.
type Checker interface {
	// Ready probes the readiness endpoint (e.g. a unix socket path). endpoint
	// empty means the strategy exposes no endpoint and is assumed ready once
	// live.
	Ready(ctx context.Context, strategy, endpoint string) Result
}

// UnixSocketChecker performs an HTTP GET /healthz over a unix-domain socket.
type UnixSocketChecker struct {
	Path    string // request path, default "/healthz"
	Timeout time.Duration
}

// Ready dials the unix socket and treats HTTP 2xx as ready.
func (c UnixSocketChecker) Ready(ctx context.Context, strategy, endpoint string) Result {
	if endpoint == "" {
		return Result{Status: pb.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "NoEndpoint"}
	}
	path := c.Path
	if path == "" {
		path = "/healthz"
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", endpoint)
			},
		},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return Result{Status: pb.ConditionStatus_CONDITION_STATUS_UNKNOWN, Reason: "BadRequest", Message: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: pb.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Unreachable", Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Result{Status: pb.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Healthy"}
	}
	return Result{Status: pb.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "NotReady"}
}

// AlwaysReady is a Checker that reports ready; useful as a default and in tests.
type AlwaysReady struct{}

// Ready always returns TRUE.
func (AlwaysReady) Ready(context.Context, string, string) Result {
	return Result{Status: pb.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AlwaysReady"}
}
