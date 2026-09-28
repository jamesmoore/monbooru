//go:build tagger

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

func runWorker(argv []string) {
	fs := flag.NewFlagSet("tagger-worker", flag.ExitOnError)
	addr := fs.String("addr", "", "parent TCP address to connect to")
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintf(os.Stderr, "tagger-worker: %v\n", err)
		os.Exit(2)
	}
	if *addr == "" {
		fmt.Fprintf(os.Stderr, "tagger-worker: --addr is required\n")
		os.Exit(2)
	}
	tagger.UseInprocBackend()
	logx.Set(os.Getenv("MONBOORU_TAGGER_WORKER_LOG"))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer cancel()

	if err := tagger.RunWorkerServer(ctx, *addr); err != nil {
		fmt.Fprintf(os.Stderr, "tagger-worker: %v\n", err)
		os.Exit(1)
	}
}
