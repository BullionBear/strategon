package health

import (
	"context"
	"net/http"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// EndpointChecker probes HTTP URLs or unix-socket paths.
type EndpointChecker struct {
	Unix    UnixSocketChecker
	Timeout time.Duration
}

// Ready dispatches on the endpoint scheme. Empty endpoint is NoEndpoint → TRUE
// so specs without a probe keep today's behaviour.
func (c EndpointChecker) Ready(ctx context.Context, strategy, endpoint string) Result {
	if endpoint == "" {
		return Result{Status: pb.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "NoEndpoint"}
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return c.httpReady(ctx, endpoint)
	}
	return c.Unix.Ready(ctx, strategy, endpoint)
}

func (c EndpointChecker) httpReady(ctx context.Context, endpoint string) Result {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
