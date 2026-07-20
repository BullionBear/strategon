// Package stream is the agent's outbound gRPC stream client. It dials the
// control plane (agent-initiated outbound connection), registers, routes
// inbound DesiredState snapshots to the reconciler, forwards northbound
// messages (StatusReport/Event) from the reconciler, and emits periodic
// heartbeats. On disconnect it reconnects with backoff; because convergence is
// level-triggered, reconnect simply re-receives the full snapshot and re-diffs.
package stream

import (
	"context"
	"log/slog"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/filebrowse"
	"github.com/bullionbear/strategon/internal/clock"
)

// Client is the agent-side stream client.
type Client struct {
	Register    *pb.Register
	Client      strategyplatformv1connect.AgentServiceClient
	Out         <-chan *pb.AgentMessage // northbound messages from the reconciler
	Submit      func(*pb.DesiredState)  // deliver DesiredState to the reconciler
	ObservedGen func() int64            // for heartbeat stamping
	// Artifacts provides StrategyDir for WorkDir browse/fetch. Optional; if
	// nil, ListDir/FetchFiles are Nack'd.
	Artifacts *artifact.Manager
	// Resources / Processes supply the latest instantaneous telemetry snapshot
	// for Heartbeat (sampled off the reconciler critical path).
	Resources  func() *pb.MachineResources
	Processes  func() []*pb.ProcessMetrics
	Clock      clock.Clock
	Heartbeat  time.Duration
	MaxBackoff time.Duration
	Logger     *slog.Logger

	// transferSem bounds concurrent browse/fetch handlers (created lazily).
	transferOnce sync.Once
	transferSem  chan struct{}
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

func (c *Client) sem() chan struct{} {
	c.transferOnce.Do(func() {
		c.transferSem = make(chan struct{}, filebrowse.MaxConcurrentTransfers)
	})
	return c.transferSem
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

	// Dedicated send path for filebrowse replies — blocks under backpressure
	// (unlike reconciler Out which drops on full).
	fileSend := make(chan *pb.AgentMessage, 16)
	sendFile := func(ctx context.Context, msg *pb.AgentMessage) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fileSend <- msg:
			return nil
		}
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			c.handleControl(ctx, msg, sendFile)
		}
	}()

	hb := c.Clock.Ticker(c.heartbeatInterval())
	defer hb.Stop()
	for {
		// Prefer heartbeats when due so large FileChunk transfers cannot starve them.
		select {
		case <-hb.C():
			if err := stream.Send(c.buildHeartbeat()); err != nil {
				_ = stream.CloseRequest()
				return err
			}
			continue
		default:
		}

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
		case msg := <-fileSend:
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

func (c *Client) handleControl(ctx context.Context, msg *pb.ControlMessage, send filebrowse.SendFunc) {
	switch p := msg.GetPayload().(type) {
	case *pb.ControlMessage_DesiredState:
		c.Submit(p.DesiredState)
	case *pb.ControlMessage_Ack:
		// no-op for the foundation
	case *pb.ControlMessage_TriggerRollback, *pb.ControlMessage_DrainNow:
		// Imperative commands are latency optimizations; DesiredState is truth.
	case *pb.ControlMessage_LeaseResponse:
		// Lease is owned by the strategy SDK via LeaseService.
	case *pb.ControlMessage_ListDir:
		c.handleListDir(ctx, p.ListDir, send)
	case *pb.ControlMessage_FetchFiles:
		c.handleFetchFiles(ctx, p.FetchFiles, send)
	default:
		_ = send(ctx, &pb.AgentMessage{
			Payload: &pb.AgentMessage_Nack{Nack: &pb.Nack{
				InReplyTo:    msg.GetMessageId(),
				Reason:       "UnknownCommand",
				AgentVersion: c.Register.GetAgentVersion(),
			}},
		})
	}
}

func (c *Client) handleListDir(ctx context.Context, req *pb.ListDir, send filebrowse.SendFunc) {
	go func() {
		sem := c.sem()
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		defer func() { <-sem }()

		listing := c.listDir(req)
		_ = send(ctx, &pb.AgentMessage{
			Payload: &pb.AgentMessage_DirListing{DirListing: listing},
		})
	}()
}

func (c *Client) listDir(req *pb.ListDir) *pb.DirListing {
	out := &pb.DirListing{RequestId: req.GetRequestId(), Path: req.GetPath()}
	if c.Artifacts == nil {
		out.Error = "file browse not configured"
		return out
	}
	if err := filebrowse.ValidateStrategy(req.GetStrategy()); err != nil {
		out.Error = err.Error()
		return out
	}
	root, err := filebrowse.Root(c.Artifacts.StrategyDir(req.GetStrategy()))
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer root.Close()
	return filebrowse.List(root, req.GetRequestId(), req.GetPath())
}

func (c *Client) handleFetchFiles(ctx context.Context, req *pb.FetchFiles, send filebrowse.SendFunc) {
	go func() {
		sem := c.sem()
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		defer func() { <-sem }()

		if c.Artifacts == nil {
			_ = send(ctx, &pb.AgentMessage{
				Payload: &pb.AgentMessage_FileChunk{FileChunk: &pb.FileChunk{
					RequestId: req.GetRequestId(),
					Error:     "file browse not configured",
					Eof:       true,
				}},
			})
			return
		}
		if err := filebrowse.ValidateStrategy(req.GetStrategy()); err != nil {
			_ = send(ctx, &pb.AgentMessage{
				Payload: &pb.AgentMessage_FileChunk{FileChunk: &pb.FileChunk{
					RequestId: req.GetRequestId(),
					Error:     err.Error(),
					Eof:       true,
				}},
			})
			return
		}
		root, err := filebrowse.Root(c.Artifacts.StrategyDir(req.GetStrategy()))
		if err != nil {
			_ = send(ctx, &pb.AgentMessage{
				Payload: &pb.AgentMessage_FileChunk{FileChunk: &pb.FileChunk{
					RequestId: req.GetRequestId(),
					Error:     err.Error(),
					Eof:       true,
				}},
			})
			return
		}
		defer root.Close()
		if err := filebrowse.Fetch(ctx, root, req.GetStrategy(), req.GetRequestId(), req.GetPaths(), send); err != nil {
			c.logger().Warn("fetch files failed", "request_id", req.GetRequestId(), "err", err)
		}
	}()
}

func (c *Client) buildHeartbeat() *pb.AgentMessage {
	var obs int64
	if c.ObservedGen != nil {
		obs = c.ObservedGen()
	}
	hb := &pb.Heartbeat{
		ObservedGeneration: obs,
		AgentVersion:       c.Register.GetAgentVersion(),
		AgentBuildVersion:  c.Register.GetAgentBuildVersion(),
	}
	if c.Resources != nil {
		hb.Resources = c.Resources()
	}
	if c.Processes != nil {
		hb.Processes = c.Processes()
	}
	return &pb.AgentMessage{
		Payload: &pb.AgentMessage_Heartbeat{Heartbeat: hb},
	}
}
