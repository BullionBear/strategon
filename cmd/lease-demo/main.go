// Command lease-demo is a sample strategy process that holds a fencing lease
// and periodically calls CheckBeforeOrder. Deploy it as a binary artifact to
// exercise LeaseService end-to-end.
//
//	go build -o /tmp/lease-demo ./cmd/lease-demo
//	# register + deploy, with env or flags for control plane URL
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bullionbear/strategon/sdk/lease"
)

func main() {
	base := flag.String("control-plane", envOr("STRATEGON_CONTROL_PLANE", "http://127.0.0.1:8080"), "LeaseService base URL (agent port)")
	machineID := flag.String("machine-id", envOr("STRATEGON_MACHINE_ID", "m1"), "machine id")
	strategy := flag.String("strategy", envOr("STRATEGON_STRATEGY", "s"), "strategy name")
	ttl := flag.Duration("ttl", 30*time.Second, "lease TTL")
	margin := flag.Duration("margin-agent", lease.DefaultMarginAgent, "local deadline margin")
	tick := flag.Duration("tick", time.Second, "CheckBeforeOrder interval (simulated orders)")
	tlsCert := flag.String("tls-cert", "", "client cert PEM")
	tlsKey := flag.String("tls-key", "", "client key PEM")
	serverCA := flag.String("server-ca", "", "server CA PEM")
	flag.Parse()

	client, err := lease.New(lease.Config{
		BaseURL:      *base,
		MachineID:    *machineID,
		Strategy:     *strategy,
		TTL:          *ttl,
		MarginAgent:  *margin,
		TLSCertFile:  *tlsCert,
		TLSKeyFile:   *tlsKey,
		ServerCAFile: *serverCA,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := client.Acquire(ctx); err != nil {
		log.Fatalf("acquire: %v", err)
	}
	log.Printf("lease acquired id=%s expires=%s", client.LeaseID(), client.ExpiresAt().Format(time.RFC3339))
	client.StartRenewLoop(ctx)

	ticker := time.NewTicker(*tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return
		case <-ticker.C:
			if err := client.CheckBeforeOrder(); err != nil {
				fmt.Fprintf(os.Stderr, "CheckBeforeOrder failed: %v — exiting (LeaseSuicide)\n", err)
				os.Exit(1)
			}
			log.Printf("order check ok (lease still held)")
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
