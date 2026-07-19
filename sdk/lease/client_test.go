package lease_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/controlplane/lease"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	sdkleas "github.com/bullionbear/strategon/sdk/lease"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestSDKAcquireCheckAndSecondMachineDenied(t *testing.T) {
	st := store.NewMemory(nil)
	mux := http.NewServeMux()
	path, h := strategyplatformv1connect.NewLeaseServiceHandler(lease.New(st, nil))
	mux.Handle(path, h)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	t.Cleanup(ts.Close)

	c1, err := sdkleas.New(sdkleas.Config{
		BaseURL: ts.URL, MachineID: "m1", Strategy: "s",
		TTL: 30 * time.Second, MarginAgent: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	ctx := context.Background()
	if err := c1.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c1.CheckBeforeOrder(); err != nil {
		t.Fatal(err)
	}

	c2, err := sdkleas.New(sdkleas.Config{
		BaseURL: ts.URL, MachineID: "m2", Strategy: "s", TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Acquire(ctx); err == nil {
		t.Fatal("m2 acquire should fail")
	}
}
