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

func cmdVolume(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageError{msg: "volume requires create|ls|rm"}
	}
	switch args[0] {
	case "create":
		return cmdVolumeCreate(args[1:])
	case "ls", "list":
		return cmdVolumeList(args[1:])
	case "rm", "delete":
		return cmdVolumeDelete(args[1:])
	default:
		return usageError{msg: fmt.Sprintf("unknown volume command %q (want create|ls|rm)", args[0])}
	}
}

func parseVolumeCmd(name string, args []string) (cliConfig, []string, error) {
	fs := flag.NewFlagSet("volume "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return cfg, nil, usageError{msg: err.Error()}
	}
	return cfg, fs.Args(), nil
}

func cmdVolumeCreate(args []string) error {
	cfg, rest, err := parseVolumeCmd("create", args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return usageError{msg: "volume create MACHINE NAME is required"}
	}
	machine, name := strings.TrimSpace(rest[0]), strings.TrimSpace(rest[1])
	resp, err := newClient(cfg).CreateVolume(context.Background(), connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: machine,
		Name:      name,
	}))
	if err != nil {
		return err
	}
	fmt.Printf("volume/%s created on %s generation=%d\n", name, machine, resp.Msg.GetGeneration())
	return nil
}

func cmdVolumeList(args []string) error {
	cfg, rest, err := parseVolumeCmd("ls", args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		return usageError{msg: "volume ls MACHINE is required"}
	}
	return listVolumes(context.Background(), newClient(cfg), rest[0], os.Stdout)
}

func cmdVolumeDelete(args []string) error {
	cfg, rest, err := parseVolumeCmd("rm", args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return usageError{msg: "volume rm MACHINE NAME is required"}
	}
	machine, name := strings.TrimSpace(rest[0]), strings.TrimSpace(rest[1])
	resp, err := newClient(cfg).DeleteVolume(context.Background(), connect.NewRequest(&pb.DeleteVolumeRequest{
		MachineId: machine,
		Name:      name,
	}))
	if err != nil {
		return err
	}
	fmt.Printf("volume/%s deleted on %s generation=%d\n", name, machine, resp.Msg.GetGeneration())
	return nil
}

func listVolumes(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, machine string, w io.Writer) error {
	resp, err := client.ListVolumes(ctx, connect.NewRequest(&pb.ListVolumesRequest{MachineId: machine}))
	if err != nil {
		return err
	}
	vols := resp.Msg.GetVolumes()
	if len(vols) == 0 {
		fmt.Fprintf(w, "No volumes on %s\n", machine)
		return nil
	}
	fmt.Fprintf(w, "NAME\tREADY\tMOUNTED BY\tPINNED BY\n")
	for _, v := range vols {
		ready := "false"
		if v.GetReady() {
			ready = "true"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			v.GetName(), ready,
			strings.Join(v.GetMountedBy(), ","),
			strings.Join(v.GetPinnedBy(), ","))
	}
	return nil
}
