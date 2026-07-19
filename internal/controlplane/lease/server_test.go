package lease_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/controlplane/lease"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestAcquireRenewSecondMachineDenied(t *testing.T) {
	st := store.NewMemory(nil)
	srv := lease.New(st, nil)
	mux := http.NewServeMux()
	path, h := strategyplatformv1connect.NewLeaseServiceHandler(srv)
	mux.Handle(path, h)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	t.Cleanup(ts.Close)

	client := strategyplatformv1connect.NewLeaseServiceClient(http.DefaultClient, ts.URL)
	ctx := context.Background()

	acq, err := client.Acquire(ctx, connect.NewRequest(&pb.LeaseRequest{
		RequestId: "r1", MachineId: "m1", Strategy: "s", RequestedTtlSeconds: 30,
	}))
	if err != nil || !acq.Msg.GetGranted() {
		t.Fatalf("acquire: %+v err=%v", acq, err)
	}

	denied, err := client.Acquire(ctx, connect.NewRequest(&pb.LeaseRequest{
		RequestId: "r2", MachineId: "m2", Strategy: "s", RequestedTtlSeconds: 30,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if denied.Msg.GetGranted() {
		t.Fatal("m2 should be denied")
	}

	renewed, err := client.Renew(ctx, connect.NewRequest(&pb.LeaseRenew{
		LeaseId: acq.Msg.GetLeaseId(), Strategy: "s",
	}))
	if err != nil || !renewed.Msg.GetGranted() {
		t.Fatalf("renew: %+v err=%v", renewed, err)
	}
	if renewed.Msg.GetExpiresAt() == nil {
		t.Fatal("renew should set expires_at")
	}
	_ = time.Second
}
