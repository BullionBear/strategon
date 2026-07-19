// Package stream is the agent's outbound gRPC stream client. It dials the
// control plane (agent-initiated, ARCHITECTURE.md §4.1), registers, routes
// inbound DesiredState snapshots to the reconciler, forwards northbound
// messages (StatusReport/Event) from the reconciler, and emits periodic
// heartbeats. On disconnect it reconnects with backoff; because convergence is
// level-triggered, reconnect simply re-receives the full snapshot and re-diffs
// (RECONCILER.md §0, ARCHITECTURE §6.3).
package stream

import (
	"context"
	"log/slog"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/clock"
)

// Client is the agent-side stream client.
type Client struct {
	Register     *pb.Register
	Client       strategyplatformv1connect.AgentServiceClient
	Out          <-chan *pb.AgentMessage // northbound messages from the reconciler
	Submit       func(*pb.DesiredState)  // deliver DesiredState to the reconciler
	ObservedGen  func() int64            // for heartbeat stamping
	Clock        clock.Clock
	Heartbeat    time.Duration
	MaxBackoff   time.Duration
	Logger       *slog.Logger
}

func (c *Client) heartbeatInterval() time.Duration {
	if c.Heartbeat <= 0 {
		return 5 * time.Second
	}
	return c.Heartbeat
}

func (c *Client) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// Run maintains the stream until ctx is cancelled, reconnecting with capped
// exponential backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := c.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logger().Warn("stream session ended, reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session runs a single connected stream lifetime.
func (c *Client) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream := c.Client.Connect(ctx)
	if err := stream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_Register{Register: c.Register},
	}); err != nil {
		return err
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			c.handleControl(msg)
		}
	}()

	hb := c.Clock.Ticker(c.heartbeatInterval())
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseRequest()
			return ctx.Err()
		case err := <-recvErr:
			_ = stream.CloseRequest()
			return err
		case msg := <-c.Out:
			if err := stream.Send(msg); err != nil {
				_ = stream.CloseRequest()
				return err
			}
		case <-hb.C():
			if err := stream.Send(c.buildHeartbeat()); err != nil {
				_ = stream.CloseRequest()
				return err
			}
		}
	}
}

func (c *Client) handleControl(msg *pb.ControlMessage) {
	switch p := msg.GetPayload().(type) {
	case *pb.ControlMessage_DesiredState:
		c.Submit(p.DesiredState)
	case *pb.ControlMessage_Ack:
		// no-op for the foundation
	case *pb.ControlMessage_TriggerRollback, *pb.ControlMessage_DrainNow:
		// Imperative commands are latency optimizations; DesiredState is truth.
	case *pb.ControlMessage_LeaseResponse:
		// Lease is owned by the strategy SDK via LeaseService (IMPROVEMENT A1).
	}
}

func (c *Client) buildHeartbeat() *pb.AgentMessage {
	var obs int64
	if c.ObservedGen != nil {
		obs = c.ObservedGen()
	}
	return &pb.AgentMessage{
		Payload: &pb.AgentMessage_Heartbeat{Heartbeat: &pb.Heartbeat{
			ObservedGeneration: obs,
			AgentVersion:       c.Register.GetAgentVersion(),
		}},
	}
}
