package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
)

func getAssignmentSet(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, name string, w io.Writer) error {
	resp, err := client.GetAssignmentSet(ctx, connect.NewRequest(&pb.GetAssignmentSetRequest{Name: name}))
	if err != nil {
		return err
	}
	printCluster(w, resp.Msg)
	return nil
}

func listAssignmentSets(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, w io.Writer) error {
	resp, err := client.ListAssignmentSets(ctx, connect.NewRequest(&pb.ListAssignmentSetsRequest{}))
	if err != nil {
		return err
	}
	if len(resp.Msg.GetSets()) == 0 {
		fmt.Fprintln(w, "No AssignmentSets.")
		return nil
	}
	for i, c := range resp.Msg.GetSets() {
		if i > 0 {
			fmt.Fprintln(w)
		}
		printCluster(w, c)
	}
	return nil
}

func printCluster(w io.Writer, c *pb.AssignmentSet) {
	meta := c.GetMetadata()
	spec := c.GetSpec()
	st := c.GetStatus()
	fmt.Fprintf(w, "AssignmentSet %s\n", meta.GetName())
	fmt.Fprintf(w, "  uid: %s\n", meta.GetUid())
	fmt.Fprintf(w, "  generation: %d\n", meta.GetGeneration())
	fmt.Fprintf(w, "  observedGeneration: %d\n", st.GetObservedGeneration())
	fmt.Fprintf(w, "  phase: %s\n", emptyDash(st.GetPhase()))
	if st.GetReason() != "" {
		fmt.Fprintf(w, "  reason: %s\n", st.GetReason())
	}
	if st.GetMessage() != "" {
		fmt.Fprintf(w, "  message: %s\n", st.GetMessage())
	}
	fmt.Fprintf(w, "  artifact: %s\n", spec.GetArtifactVersion())
	if spec.GetConfigVersion() != "" {
		fmt.Fprintf(w, "  config: %s\n", spec.GetConfigVersion())
	}
	fmt.Fprintf(w, "  strategy: %s\n", emptyDash(spec.GetStrategy()))
	if k := st.GetAssignmentKey(); k != "" {
		fmt.Fprintf(w, "  assignmentKey: %s\n", k)
	}
	if u := spec.GetUpdate(); u != nil {
		fmt.Fprintf(w, "  update: maxUnavailable=%d waitReadySeconds=%d\n", u.GetMaxUnavailable(), u.GetWaitReadySeconds())
	}
	statusByMember := map[string]*pb.MemberStatus{}
	for _, s := range st.GetMembers() {
		statusByMember[s.GetMachine()+"\x00"+s.GetName()] = s
	}
	fmt.Fprintf(w, "  servers:\n")
	for _, srv := range spec.GetMembers() {
		ss := statusByMember[srv.GetMachine()+"\x00"+srv.GetName()]
		phase, ready, conv := "—", false, false
		if ss != nil {
			phase = emptyDash(ss.GetPhase())
			ready = ss.GetReady()
			conv = ss.GetConverged()
		}
		fmt.Fprintf(w, "    %s  %s  phase=%s ready=%t converged=%t\n",
			srv.GetMachine(), srv.GetName(), phase, ready, conv)
	}
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
