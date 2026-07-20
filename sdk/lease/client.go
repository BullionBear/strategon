// Package lease is the strategy-side fencing-lease SDK.
// Strategies acquire/renew via LeaseService and must call CheckBeforeOrder
// before trading. The agent process does not participate.
package lease

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/mtls"
	"golang.org/x/net/http2"
)

// DefaultMarginAgent is the strategy-side lease margin: the local deadline
// expires this much before the control-plane expires_at.
const DefaultMarginAgent = time.Second

// Config configures a Client.
type Config struct {
	BaseURL     string // e.g. http://127.0.0.1:8080 or https://...
	MachineID   string
	Strategy    string
	TTL         time.Duration
	MarginAgent time.Duration

	// Optional mTLS (all three required together).
	TLSCertFile  string
	TLSKeyFile   string
	ServerCAFile string

	// ClockJumpThreshold: if wall vs monotonic advance diverges by more than
	// this between checks, CheckBeforeOrder fails (suspend/resume heuristic).
	ClockJumpThreshold time.Duration
}

// Client holds a fencing lease and renews it in the background.
type Client struct {
	cfg    Config
	rpc    strategyplatformv1connect.LeaseServiceClient
	margin time.Duration

	mu           sync.Mutex
	leaseID      string
	deadlineMono time.Duration // max time.Since(anchor) before CheckBeforeOrder fails
	anchor       time.Time     // time.Now() at last grant (monotonic-capable)
	expiresAt    time.Time
	lastWall     time.Time
	lastElapsed  time.Duration
	stopRenew    context.CancelFunc
}

// New builds a Client. Call Acquire before CheckBeforeOrder / StartRenewLoop.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" || cfg.MachineID == "" || cfg.Strategy == "" {
		return nil, errors.New("lease: BaseURL, MachineID, and Strategy are required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Second
	}
	margin := cfg.MarginAgent
	if margin <= 0 {
		margin = DefaultMarginAgent
	}
	if cfg.ClockJumpThreshold <= 0 {
		cfg.ClockJumpThreshold = 2 * time.Second
	}
	httpClient, err := httpClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:    cfg,
		rpc:    strategyplatformv1connect.NewLeaseServiceClient(httpClient, cfg.BaseURL),
		margin: margin,
	}, nil
}

func httpClient(cfg Config) (*http.Client, error) {
	tlsMode := cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" || cfg.ServerCAFile != ""
	if !tlsMode {
		return &http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		}}, nil
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" || cfg.ServerCAFile == "" {
		return nil, errors.New("lease: mTLS requires TLSCertFile, TLSKeyFile, and ServerCAFile")
	}
	cert, err := mtls.LoadCert(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	ca, err := mtls.LoadCAPool(cfg.ServerCAFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http2.Transport{
		TLSClientConfig: mtls.ClientConfig(cert, ca),
	}}, nil
}

// Acquire obtains a lease from the control plane.
func (c *Client) Acquire(ctx context.Context) error {
	resp, err := c.rpc.Acquire(ctx, connect.NewRequest(&pb.LeaseRequest{
		RequestId:           fmt.Sprintf("%s-%d", c.cfg.MachineID, time.Now().UnixNano()),
		MachineId:           c.cfg.MachineID,
		Strategy:            c.cfg.Strategy,
		RequestedTtlSeconds: int32(c.cfg.TTL / time.Second),
	}))
	if err != nil {
		return err
	}
	if !resp.Msg.GetGranted() {
		return fmt.Errorf("lease denied: %s", resp.Msg.GetDenyReason())
	}
	c.applyGrant(resp.Msg.GetLeaseId(), resp.Msg.GetExpiresAt().AsTime())
	return nil
}

// StartRenewLoop renews until ctx is cancelled. Call after successful Acquire.
func (c *Client) StartRenewLoop(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.stopRenew != nil {
		c.stopRenew()
	}
	c.stopRenew = cancel
	c.mu.Unlock()

	go func() {
		ticker := time.NewTicker(c.renewInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.renewOnce(ctx); err != nil {
					return
				}
			}
		}
	}()
}

// Close stops the renew loop.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopRenew != nil {
		c.stopRenew()
		c.stopRenew = nil
	}
}

// CheckBeforeOrder verifies the local lease is still valid before trading.
// Call synchronously on the order path; on error do not send the order.
func (c *Client) CheckBeforeOrder() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaseID == "" {
		return errors.New("lease: not acquired")
	}
	nowWall := time.Now()
	elapsed := time.Since(c.anchor)
	if elapsed >= c.deadlineMono {
		return errors.New("lease: local deadline expired")
	}
	if !c.lastWall.IsZero() {
		dWall := nowWall.Sub(c.lastWall)
		dMono := elapsed - c.lastElapsed
		delta := dWall - dMono
		if delta < 0 {
			delta = -delta
		}
		if delta > c.cfg.ClockJumpThreshold {
			return fmt.Errorf("lease: clock jump detected (%v)", delta)
		}
	}
	c.lastWall = nowWall
	c.lastElapsed = elapsed
	return nil
}

// LeaseID returns the current lease id (empty if none).
func (c *Client) LeaseID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaseID
}

// ExpiresAt returns the last CP-reported expiry (wall clock).
func (c *Client) ExpiresAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiresAt
}

func (c *Client) renewInterval() time.Duration {
	d := c.cfg.TTL / 3
	if d < time.Second {
		d = time.Second
	}
	return d
}

func (c *Client) renewOnce(ctx context.Context) error {
	c.mu.Lock()
	id := c.leaseID
	strategy := c.cfg.Strategy
	c.mu.Unlock()
	if id == "" {
		return errors.New("lease: not acquired")
	}
	resp, err := c.rpc.Renew(ctx, connect.NewRequest(&pb.LeaseRenew{
		LeaseId:  id,
		Strategy: strategy,
	}))
	if err != nil {
		return err
	}
	if !resp.Msg.GetGranted() {
		return fmt.Errorf("lease renew denied: %s", resp.Msg.GetDenyReason())
	}
	c.applyGrant(resp.Msg.GetLeaseId(), resp.Msg.GetExpiresAt().AsTime())
	return nil
}

func (c *Client) applyGrant(leaseID string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.leaseID = leaseID
	c.expiresAt = expiresAt
	until := expiresAt.Sub(now) - c.margin
	if until < 0 {
		until = 0
	}
	if c.anchor.IsZero() {
		// First acquire: establish mono anchor and jump baseline so the first
		// CheckBeforeOrder does not skip clock-jump detection.
		c.anchor = now
		c.deadlineMono = until
		c.lastWall = now
		c.lastElapsed = 0
		return
	}
	// Renew: extend the mono deadline relative to the existing anchor. Do NOT
	// clear lastWall/lastElapsed — resetting them opened a window where the
	// first post-renew CheckBeforeOrder skipped jump detection (suspend blind spot).
	c.deadlineMono = time.Since(c.anchor) + until
}
