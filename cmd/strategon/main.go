package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if isUsage(err) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func isUsage(err error) bool {
	_, ok := err.(usageError)
	return ok
}

func peelLeadingGlobals(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--addr" && i+1 < len(args):
			os.Setenv("STRATEGON_ADDR", args[i+1])
			i++
		case strings.HasPrefix(a, "--addr="):
			os.Setenv("STRATEGON_ADDR", strings.TrimPrefix(a, "--addr="))
		case a == "--token" && i+1 < len(args):
			os.Setenv("STRATEGON_TOKEN", args[i+1])
			i++
		case strings.HasPrefix(a, "--token="):
			os.Setenv("STRATEGON_TOKEN", strings.TrimPrefix(a, "--token="))
		default:
			out = append(out, args[i:]...)
			return out
		}
	}
	return out
}

func run(args []string) error {
	args = peelLeadingGlobals(args)
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usageText)
		if len(args) == 0 {
			return usageError{msg: "command is required"}
		}
		return nil
	}
	switch args[0] {
	case "apply":
		return cmdApply(args[1:])
	case "get":
		return cmdGet(args[1:])
	case "wait":
		return cmdWait(args[1:])
	default:
		return usageError{msg: fmt.Sprintf("unknown command %q\n\n%s", args[0], usageText)}
	}
}

const usageText = `Usage: strategon <command> [flags]

Commands:
  apply -f FILE                 Apply a YAML document
  get natscluster NAME          Show one NatsCluster
  get natsclusters              List NatsClusters
  wait natscluster NAME         Block until a condition holds

Apply dispatches on kind:
  NatsCluster         → ApplyNatsCluster (does not Deploy members)
  StrategyAssignment  → ApplyAssignment

Unknown kinds exit non-zero.

Global flags (any command):
  --addr    Control plane human API (default http://127.0.0.1:8081 or $STRATEGON_ADDR)
  --token   Bearer API token (default $STRATEGON_TOKEN)
`

func addGlobal(fs *flag.FlagSet, cfg *cliConfig) {
	fs.StringVar(&cfg.Addr, "addr", envOr("STRATEGON_ADDR", defaultAddr), "control plane human API base URL")
	fs.StringVar(&cfg.Token, "token", os.Getenv("STRATEGON_TOKEN"), "Bearer API token")
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	file := fs.String("f", "", "YAML file (`-` for stdin)")
	fs.StringVar(file, "filename", "", "alias for -f")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: err.Error()}
	}
	if strings.TrimSpace(*file) == "" {
		return usageError{msg: "apply requires -f FILE"}
	}
	results, err := applyFile(context.Background(), newClient(cfg), *file)
	if err != nil {
		return err
	}
	for _, r := range results {
		fmt.Println(formatApply(r))
	}
	return nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return usageError{msg: err.Error()}
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageError{msg: "get requires a resource (natscluster)"}
	}
	res := strings.ToLower(rest[0])
	client := newClient(cfg)
	ctx := context.Background()
	switch res {
	case "natsclusters":
		return listNatsClusters(ctx, client, os.Stdout)
	case "natscluster", "nc":
		if len(rest) < 2 || strings.TrimSpace(rest[1]) == "" {
			return usageError{msg: "get natscluster requires a name"}
		}
		return getNatsCluster(ctx, client, rest[1], os.Stdout)
	default:
		return usageError{msg: fmt.Sprintf("unknown resource %q (want natscluster)", rest[0])}
	}
}

func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	forCond := fs.String("for", "ready", "condition to wait for (ready)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: err.Error()}
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return usageError{msg: "wait natscluster NAME is required"}
	}
	if strings.ToLower(rest[0]) != "natscluster" && strings.ToLower(rest[0]) != "nc" {
		return usageError{msg: fmt.Sprintf("unknown resource %q (want natscluster)", rest[0])}
	}
	name := strings.TrimSpace(rest[1])
	if name == "" {
		return usageError{msg: "wait natscluster requires a name"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Second)
	defer cancel()
	return waitNatsCluster(ctx, newClient(cfg), name, *forCond, *timeout)
}
