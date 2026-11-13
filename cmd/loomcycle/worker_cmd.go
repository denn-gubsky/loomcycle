package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
)

// runWorker is `loomcycle worker` (RFC GO): a conversion worker that dials
// the dispatcher and runs the conversions it is handed. It loads no config,
// opens no store and holds no provider key; the core it serves keeps every
// file's path and grant to itself and sends only bytes.
func runWorker(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dispatcher := fs.String("dispatcher", "", "the dispatcher's address: unix:///path/to/socket or host:port")
	id := fs.String("id", "", "this worker's name to the dispatcher (default host-pid-random)")
	parentPID := fs.Int("parent-pid", 0, "exit when this process is no longer the parent (set by the core for its children)")
	oomAdj := fs.Int("oom-score-adj", 0, "on Linux, raise this process's OOM score to this value (the core sets 1000 for its children)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	slots := 1
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKER_SLOTS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			fmt.Fprintf(stderr, "loomcycle worker: LOOMCYCLE_WORKER_SLOTS: %q is not a whole number from 1 to 64\n", v)
			return 2
		}
		slots = n
	}
	if *oomAdj != 0 && runtime.GOOS == "linux" {
		// Raising one's own score needs no privilege; a failure only means
		// the kernel weighs this worker like any other process.
		if err := os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(*oomAdj)), 0o600); err != nil {
			slog.Warn("conversion worker could not raise its OOM score", "err", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := worker.Run(ctx, worker.Options{
		Target:       *dispatcher,
		Token:        os.Getenv("LOOMCYCLE_WORKER_TOKEN"),
		Slots:        slots,
		Scratch:      os.Getenv("LOOMCYCLE_WORKER_SCRATCH"),
		ID:           *id,
		BuildVersion: buildVersion,
		ParentPID:    *parentPID,
	})
	switch {
	case err == nil:
		return 0
	case errors.Is(err, worker.ErrRefused):
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
	return 1
}
