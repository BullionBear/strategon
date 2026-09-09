package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
)

var waitPoll = 500 * time.Millisecond

func waitAssignmentSet(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, name, cond string, timeout time.Duration) error {
	want := strings.ToLower(strings.TrimSpace(cond))
	if want == "" {
		want = "ready"
	}
	if want != "ready" {
		return fmt.Errorf("unsupported --for=%s (want ready)", cond)
	}
	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.GetAssignmentSet(ctx, connect.NewRequest(&pb.GetAssignmentSetRequest{Name: name}))
		if err != nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("wait assignmentset %q: %w", name, err)
			}
		} else {
			c := resp.Msg
			phase := c.GetStatus().GetPhase()
			switch phase {
			case "Ready":
				if c.GetStatus().GetObservedGeneration() < c.GetMetadata().GetGeneration() {
					break
				}
				fmt.Printf("assignmentset/%s is Ready (generation=%d observed=%d)\n",
					name, c.GetMetadata().GetGeneration(), c.GetStatus().GetObservedGeneration())
				return nil
			case "Failed", "Degraded":
				return fmt.Errorf("assignmentset %q is %s: %s", name, phase, strings.TrimSpace(c.GetStatus().GetMessage()+" "+c.GetStatus().GetReason()))
			case "Deleting":
				return fmt.Errorf("assignmentset %q is Deleting", name)
			}
		}
		if time.Now().After(deadline) {
			phase := ""
			if err == nil && resp != nil {
				phase = resp.Msg.GetStatus().GetPhase()
			}
			return fmt.Errorf("timed out waiting for assignmentset %q to be Ready (last phase=%s)", name, emptyDash(phase))
		}
		sleep := waitPoll
		if remain := time.Until(deadline); remain < sleep {
			sleep = remain
		}
		if sleep < 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}
