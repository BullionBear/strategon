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
	case "volume":
		return cmdVolume(args[1:])
	case "files":
		return cmdFiles(args[1:])
	case "logs":
		return cmdLogs(args[1:])
	default:
		return usageError{msg: fmt.Sprintf("unknown command %q\n\n%s", args[0], usageText)}
	}
}

const usageText = `Usage: strategon <command> [flags]

Commands:
  apply -f FILE                 Apply a YAML document
  get assignmentset NAME        Show one AssignmentSet
  get assignmentsets            List AssignmentSets
  wait assignmentset NAME       Block until a condition holds
  volume create MACHINE NAME    Create a machine-level volume
  volume ls MACHINE             List volumes on a machine
  volume rm MACHINE NAME        Delete a volume (fails if mounted)
  files ls MACHINE STRATEGY [PATH]   Browse a strategy slot
  files get MACHINE STRATEGY PATH [PATH...]
                                Download slot files (use -o FILE)
  logs MACHINE STRATEGY         Fetch .stdio/payload.log (add --all for rotates)

  Apply is authoritative: omit captureStdio to turn payload stdio capture off.

Apply dispatches on kind:
  AssignmentSet       → ApplyAssignmentSet (does not Deploy members)
  StrategyAssignment  → ApplyAssignment
  MachineVolumes      → CreateVolume per name (ensure-only; never deletes)

Unknown kinds exit non-zero.

Global flags (any command):
  --addr    Control plane human API (default http://127.0.0.1:8081 or $STRATEGON_ADDR)
  --token   from $STRATEGON_TOKEN (human API)
`

func addGlobal(fs *flag.FlagSet, cfg *cliConfig) {
	fs.StringVar(&cfg.Addr, "addr", envOr("STRATEGON_ADDR", defaultAddr), "control plane human API base URL")
	fs.StringVar(&cfg.Token, "token", os.Getenv("STRATEGON_TOKEN"), "human API credential from STRATEGON_TOKEN")
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
		return usageError{msg: "get requires a resource (assignmentset)"}
	}
	res := strings.ToLower(rest[0])
	client := newClient(cfg)
	ctx := context.Background()
	switch res {
	case "assignmentsets":
		return listAssignmentSets(ctx, client, os.Stdout)
	case "assignmentset", "as":
		if len(rest) < 2 || strings.TrimSpace(rest[1]) == "" {
			return usageError{msg: "get assignmentset requires a name"}
		}
		return getAssignmentSet(ctx, client, rest[1], os.Stdout)
	default:
		return usageError{msg: fmt.Sprintf("unknown resource %q (want assignmentset)", rest[0])}
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
		return usageError{msg: "wait assignmentset NAME is required"}
	}
	if strings.ToLower(rest[0]) != "assignmentset" && strings.ToLower(rest[0]) != "as" {
		return usageError{msg: fmt.Sprintf("unknown resource %q (want assignmentset)", rest[0])}
	}
	name := strings.TrimSpace(rest[1])
	if name == "" {
		return usageError{msg: "wait assignmentset requires a name"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Second)
	defer cancel()
	return waitAssignmentSet(ctx, newClient(cfg), name, *forCond, *timeout)
}
