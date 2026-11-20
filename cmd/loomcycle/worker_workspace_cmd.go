package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	wsworker "github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// runWorkspaceWorker is `loomcycle worker --role workspace` (RFC GW): it holds
// workspaces as directories under LOOMCYCLE_WORKSPACE_ROOT and runs the ops
// the core sends. Like the conversion worker it loads no config, opens no
// store and holds no provider key; its token is an operator token holding
// only workers:attach.
func runWorkspaceWorker(dispatcher, id string, stderr io.Writer) int {
	capacity, err := envCount("LOOMCYCLE_WORKSPACE_CAPACITY", 1, 10000)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	streams, err := envCount("LOOMCYCLE_WORKSPACE_IDLE_STREAMS", 1, wsworker.MaxIdleStreams)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	commands, err := workspaceCommands(os.Getenv("LOOMCYCLE_WORKSPACE_COMMANDS"))
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	tlsCfg, err := workerTLS()
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = wsworker.Run(ctx, wsworker.Options{
		Target: dispatcher,
		// Trimmed as the runtime trims it: a secret file's trailing newline
		// must not change the token.
		Token:        strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKER_TOKEN")),
		ID:           id,
		BuildVersion: buildVersion,
		Root:         strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKSPACE_ROOT")),
		Engine:       strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKSPACE_ENGINE")),
		Capacity:     capacity,
		IdleStreams:  streams,
		Commands:     commands,
		TLS:          tlsCfg,
		Insecure:     os.Getenv("LOOMCYCLE_WORKER_INSECURE") == "1",
	})
	switch {
	case err == nil:
		return 0
	case errors.Is(err, wsworker.ErrRefused):
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
	return 1
}

// envCount reads a whole number from lo to hi from env; 0 when unset (the
// worker's default).
func envCount(env string, lo, hi int) (int, error) {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s: %q is not a whole number from %d to %d", env, v, lo, hi)
	}
	return n, nil
}

// workspaceCommands reads LOOMCYCLE_WORKSPACE_COMMANDS: a comma-separated list
// of the programs an exec may start with; empty allows any. A name holding
// whitespace or a path separator could never match a command's first word,
// so it is refused rather than silently allowing nothing.
func workspaceCommands(v string) ([]string, error) {
	var out []string
	for _, name := range strings.Split(v, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, " \t/") {
			return nil, fmt.Errorf("LOOMCYCLE_WORKSPACE_COMMANDS: %q is not a program name", name)
		}
		out = append(out, name)
	}
	return out, nil
}
