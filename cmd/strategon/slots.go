package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
)

func cmdSlots(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageError{msg: "slots requires ls|reap"}
	}
	switch args[0] {
	case "ls", "list":
		return cmdSlotsList(args[1:])
	case "reap":
		return cmdSlotsReap(args[1:])
	default:
		return usageError{msg: fmt.Sprintf("unknown slots command %q (want ls|reap)", args[0])}
	}
}

func parseSlotsCmd(name string, args []string) (cliConfig, []string, error) {
	fs := flag.NewFlagSet("slots "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return cfg, nil, usageError{msg: err.Error()}
	}
	return cfg, fs.Args(), nil
}

func cmdSlotsList(args []string) error {
	cfg, rest, err := parseSlotsCmd("ls", args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		return usageError{msg: "slots ls MACHINE is required"}
	}
	return listSlots(context.Background(), newClient(cfg), rest[0], os.Stdout)
}

func cmdSlotsReap(args []string) error {
	cfg, rest, err := parseSlotsCmd("reap", args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return usageError{msg: "slots reap MACHINE NAME [NAME…] is required"}
	}
	machine := strings.TrimSpace(rest[0])
	names := rest[1:]
	resp, err := newClient(cfg).ReapStrategies(context.Background(), connect.NewRequest(&pb.ReapStrategiesRequest{
		MachineId:  machine,
		Strategies: names,
	}))
	if err != nil {
		return err
	}
	for _, r := range resp.Msg.GetResults() {
		if r.GetRemoved() {
			fmt.Printf("%s removed freed=%d\n", r.GetStrategy(), r.GetFreedBytes())
			continue
		}
		fmt.Printf("%s error=%s\n", r.GetStrategy(), r.GetError())
	}
	return nil
}

func listSlots(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, machine string, w io.Writer) error {
	resp, err := client.ListStrategySlots(ctx, connect.NewRequest(&pb.ListStrategySlotsRequest{MachineId: machine}))
	if err != nil {
		return err
	}
	slots := resp.Msg.GetSlots()
	if len(slots) == 0 {
		fmt.Fprintf(w, "No strategy slots on %s\n", machine)
		return nil
	}
	fmt.Fprintf(w, "NAME\tASSIGNED\tVERSION\tSIZE\n")
	for _, s := range slots {
		assigned := "false"
		if s.GetAssigned() {
			assigned = "true"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", s.GetStrategy(), assigned, s.GetCurrentVersion(), s.GetSizeBytes())
	}
	fmt.Fprintf(w, "total_bytes\t%d\n", resp.Msg.GetTotalBytes())
	return nil
}
