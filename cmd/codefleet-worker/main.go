package main

import (
	"context"
	"example.com/codefleet/internal/worker"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "worker" {
		args = args[1:]
	}
	f := flag.NewFlagSet("worker", flag.ExitOnError)
	root := f.String("root", "/workspace/code", "durable code workspace")
	f.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if e := worker.Run(ctx, *root); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
