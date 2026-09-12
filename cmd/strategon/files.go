package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/agent/driver"
)

func cmdFiles(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usageError{msg: "files requires ls|get"}
	}
	switch args[0] {
	case "ls", "list":
		return cmdFilesList(args[1:])
	case "get":
		return cmdFilesGet(args[1:])
	default:
		return usageError{msg: fmt.Sprintf("unknown files command %q (want ls|get)", args[0])}
	}
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	all := fs.Bool("all", false, "also fetch rotated payload.log.1..3")
	out := fs.String("o", "", "write to FILE instead of stdout")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: err.Error()}
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return usageError{msg: "logs MACHINE STRATEGY is required"}
	}
	paths := []string{filepath.Join(driver.StdioDirName, driver.PayloadLogName)}
	if *all {
		for i := 1; i <= driver.PayloadLogArchives; i++ {
			paths = append(paths, fmt.Sprintf("%s/%s.%d", driver.StdioDirName, driver.PayloadLogName, i))
		}
	}
	return downloadFiles(context.Background(), newClient(cfg), rest[0], rest[1], paths, *out, os.Stdout)
}

func cmdFilesList(args []string) error {
	fs := flag.NewFlagSet("files ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return usageError{msg: err.Error()}
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return usageError{msg: "files ls MACHINE STRATEGY [PATH] is required"}
	}
	path := "."
	if len(rest) > 2 {
		path = rest[2]
	}
	resp, err := newClient(cfg).BrowseDir(context.Background(), connect.NewRequest(&pb.BrowseDirRequest{
		MachineId: rest[0],
		Strategy:  rest[1],
		Path:      path,
	}))
	if err != nil {
		return err
	}
	fmt.Printf("PATH\t%s\n", resp.Msg.GetPath())
	for _, e := range resp.Msg.GetEntries() {
		kind := "file"
		if e.GetIsDir() {
			kind = "dir"
		}
		fmt.Printf("%s\t%s\t%d\n", kind, e.GetName(), e.GetSize())
	}
	return nil
}

func cmdFilesGet(args []string) error {
	fs := flag.NewFlagSet("files get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg cliConfig
	addGlobal(fs, &cfg)
	out := fs.String("o", "", "write to FILE instead of stdout")
	if err := fs.Parse(args); err != nil {
		return usageError{msg: err.Error()}
	}
	rest := fs.Args()
	if len(rest) < 3 {
		return usageError{msg: "files get MACHINE STRATEGY PATH [PATH...] is required"}
	}
	return downloadFiles(context.Background(), newClient(cfg), rest[0], rest[1], rest[2:], *out, os.Stdout)
}

func downloadFiles(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, machine, strategy string, paths []string, outPath string, stdout io.Writer) error {
	stream, err := client.DownloadFiles(ctx, connect.NewRequest(&pb.DownloadFilesRequest{
		MachineId: machine,
		Strategy:  strategy,
		Paths:     paths,
	}))
	if err != nil {
		return err
	}
	var dest io.Writer = stdout
	var f *os.File
	if strings.TrimSpace(outPath) != "" {
		f, err = os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		dest = f
	}
	for stream.Receive() {
		msg := stream.Msg()
		if len(msg.GetData()) > 0 {
			if _, err := dest.Write(msg.GetData()); err != nil {
				return err
			}
		}
	}
	return stream.Err()
}
