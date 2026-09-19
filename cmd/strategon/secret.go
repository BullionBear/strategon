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

func cmdSecret(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageError{msg: "secret requires put|get|ls|rm"}
	}
	switch args[0] {
	case "put":
		return cmdSecretPut(args[1:])
	case "get":
		return cmdSecretGet(args[1:])
	case "ls", "list":
		return cmdSecretList(args[1:])
	case "rm", "delete":
		return cmdSecretDelete(args[1:])
	default:
		return usageError{msg: fmt.Sprintf("unknown secret command %q (want put|get|ls|rm)", args[0])}
	}
}

func parseSecretCmd(name string, args []string) (cliConfig, []string, error) {
	fs := flag.NewFlagSet("secret "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return cfg, nil, usageError{msg: err.Error()}
	}
	return cfg, fs.Args(), nil
}

func cmdSecretPut(args []string) error {
	cfg, rest, err := parseSecretCmd("put", args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		return usageError{msg: "secret put NAME is required (value from stdin)"}
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	val := strings.TrimRight(string(raw), "\n")
	resp, err := newClient(cfg).PutSecret(context.Background(), connect.NewRequest(&pb.PutSecretRequest{
		Name:  strings.TrimSpace(rest[0]),
		Value: val,
	}))
	if err != nil {
		return err
	}
	fmt.Println(resp.Msg.GetToken())
	return nil
}

func cmdSecretGet(args []string) error {
	cfg, rest, err := parseSecretCmd("get", args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		return usageError{msg: "secret get NAME is required"}
	}
	return printSecret(context.Background(), newClient(cfg), rest[0], os.Stdout)
}

func cmdSecretDelete(args []string) error {
	cfg, rest, err := parseSecretCmd("rm", args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		return usageError{msg: "secret rm NAME is required"}
	}
	return deleteSecret(context.Background(), newClient(cfg), strings.TrimSpace(rest[0]), os.Stdout)
}

func deleteSecret(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, name string, w io.Writer) error {
	if _, err := client.DeleteSecret(ctx, connect.NewRequest(&pb.DeleteSecretRequest{Name: name})); err != nil {
		return err
	}
	fmt.Fprintf(w, "secret/%s deleted\n", name)
	return nil
}

func cmdSecretList(args []string) error {
	cfg, _, err := parseSecretCmd("ls", args)
	if err != nil {
		return err
	}
	return listSecrets(context.Background(), newClient(cfg), os.Stdout)
}

func printSecret(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, name string, w io.Writer) error {
	resp, err := client.GetSecret(ctx, connect.NewRequest(&pb.GetSecretRequest{Name: strings.TrimSpace(name)}))
	if err != nil {
		return err
	}
	s := resp.Msg.GetSecret()
	fmt.Fprintf(w, "NAME\tTOKEN\tBYTES\tKEY_ID\n")
	fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", s.GetName(), s.GetToken(), s.GetLengthBytes(), s.GetKeyId())
	return nil
}

func listSecrets(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, w io.Writer) error {
	resp, err := client.ListSecrets(ctx, connect.NewRequest(&pb.ListSecretsRequest{}))
	if err != nil {
		return err
	}
	secs := resp.Msg.GetSecrets()
	if len(secs) == 0 {
		fmt.Fprintln(w, "No secrets")
		return nil
	}
	fmt.Fprintf(w, "NAME\tTOKEN\tBYTES\tKEY_ID\n")
	for _, s := range secs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", s.GetName(), s.GetToken(), s.GetLengthBytes(), s.GetKeyId())
	}
	return nil
}
