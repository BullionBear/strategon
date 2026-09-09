package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestEndpointCheckerHTTP(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	c := EndpointChecker{}
	ctx := context.Background()
	if got := c.Ready(ctx, "s", ""); got.Status != pb.ConditionStatus_CONDITION_STATUS_TRUE || got.Reason != "NoEndpoint" {
		t.Fatalf("empty = %+v", got)
	}
	if got := c.Ready(ctx, "s", ok.URL+"/healthz"); got.Status != pb.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Fatalf("200 = %+v", got)
	}
	if got := c.Ready(ctx, "s", bad.URL+"/healthz"); got.Status != pb.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Fatalf("503 = %+v", got)
	}
	if got := c.Ready(ctx, "s", "http://127.0.0.1:1/healthz"); got.Status != pb.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Fatalf("unreachable = %+v", got)
	}
}
