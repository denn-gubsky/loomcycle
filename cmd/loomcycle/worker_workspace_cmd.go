package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	convworker "github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	wsworker "github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// runWorkspaceWorker is `loomcycle worker --role workspace` (RFC GW): it holds
// workspaces as directories under LOOMCYCLE_WORKSPACE_ROOT and runs the ops
// the core sends. Like the conversion worker it loads no config, opens no
// store and holds no provider key; its token is an operator token holding
// only workers:attach.
func runWorkspaceWorker(dispatcher, id string, parentPID int, stderr io.Writer) int {
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
	convertMem, err := envCount("LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB", 64, 1<<20)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	tlsCfg, err := workerTLS()
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	container, err := workspaceContainerOptions()
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle worker: %v\n", err)
		return 2
	}
	child, labels := workspaceConvertChild(convertMem)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = wsworker.Run(ctx, wsworker.Options{
		Target: dispatcher,
		// Trimmed as the runtime trims it: a secret file's trailing newline
		// must not change the token.
		Token:        strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKER_TOKEN")),
		ID:           id,
		ParentPID:    parentPID,
		BuildVersion: buildVersion,
		Root:         strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKSPACE_ROOT")),
		Engine:       strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKSPACE_ENGINE")),
		Container:    container,
		// Not a boundary (engine.Host): every process on the host reaches
		// a workspace's port while the workspace is held here.
		HostEgress:   os.Getenv("LOOMCYCLE_WORKSPACE_HOST_EGRESS") == "1",
		Capacity:     capacity,
		IdleStreams:  streams,
		Commands:     commands,
		ConvertChild: child,
		Labels:       labels,
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

// workspaceConvertChild is the conversion child a workspace worker starts
// (RFC GW D34): this binary, run as a conversion worker with the helper
// commands of this process's environment, and the labels it will report.
// nil when this binary cannot be found: the worker then converts nothing.
func workspaceConvertChild(memoryLimitMB int) (*wsworker.ConvertChild, []string) {
	bin, err := os.Executable()
	if err != nil {
		slog.Warn("workspace worker: cannot find this binary; it converts no workspace file", "err", err)
		return nil, nil
	}
	env := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	child := &wsworker.ConvertChild{
		Binary: bin, MemoryLimitMB: memoryLimitMB,
		Rasterize: env("LOOMCYCLE_WORKER_RASTERIZE_COMMAND"), Media: env("LOOMCYCLE_WORKER_MEDIA_COMMAND"),
		Encode: env("LOOMCYCLE_WORKER_ENCODE_COMMAND"), WritePDF: env("LOOMCYCLE_WORKER_WRITE_COMMAND"),
		Draw: env("LOOMCYCLE_WORKER_MERMAID_COMMAND"), Wrapper: env("LOOMCYCLE_HELPER_WRAPPER"),
		DjVuText: env("LOOMCYCLE_WORKER_DJVU_TEXT_COMMAND"), DjVuInspect: env("LOOMCYCLE_WORKER_DJVU_INSPECT_COMMAND"),
		DjVuRasterize: env("LOOMCYCLE_WORKER_DJVU_RASTERIZE_COMMAND"),
	}
	return child, convworker.Labels(workerHelpers(), slog.Default())
}

// workspaceContainerOptions reads the container engine's settings
// (LOOMCYCLE_WORKSPACE_CONTAINER_*, RFC GW P3). They are read whatever the
// engine, so a malformed one fails the start rather than a later switch to
// the engine; the engine checks the image and the runtime itself.
func workspaceContainerOptions() (engine.ContainerOptions, error) {
	env := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	mem, err := envCount("LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB", 64, 1<<20)
	if err != nil {
		return engine.ContainerOptions{}, err
	}
	pids, err := envCount("LOOMCYCLE_WORKSPACE_CONTAINER_PIDS", 16, 1<<22)
	if err != nil {
		return engine.ContainerOptions{}, err
	}
	tmp, err := envCount("LOOMCYCLE_WORKSPACE_CONTAINER_TMP_MB", 1, 1<<20)
	if err != nil {
		return engine.ContainerOptions{}, err
	}
	var cpus float64
	if v := env("LOOMCYCLE_WORKSPACE_CONTAINER_CPUS"); v != "" {
		cpus, err = strconv.ParseFloat(v, 64)
		if err != nil || cpus < 0.01 || cpus > 1024 {
			return engine.ContainerOptions{}, fmt.Errorf("LOOMCYCLE_WORKSPACE_CONTAINER_CPUS: %q is not a number of CPUs from 0.01 to 1024", v)
		}
	}
	hostRoot := env("LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT")
	if hostRoot != "" && !filepath.IsAbs(hostRoot) {
		return engine.ContainerOptions{}, fmt.Errorf("LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT: %q is not an absolute path", hostRoot)
	}
	// Egress (RFC GW P4): a session gets a way out through the worker only
	// when the operator names where its sockets live; unset, it has no
	// network, as in P3.
	var eg *engine.ContainerEgress
	if dir := env("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR"); dir != "" {
		hostDir := env("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_HOST_DIR")
		if !filepath.IsAbs(dir) || (hostDir != "" && !filepath.IsAbs(hostDir)) {
			return engine.ContainerOptions{}, fmt.Errorf("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR %q and _EGRESS_HOST_DIR %q must be absolute paths", dir, hostDir)
		}
		eg = &engine.ContainerEgress{Dir: dir, HostDir: hostDir, Image: env("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_IMAGE")}
	} else if env("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_HOST_DIR") != "" || env("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_IMAGE") != "" {
		return engine.ContainerOptions{}, errors.New("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_HOST_DIR and _EGRESS_IMAGE need LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR")
	}
	return engine.ContainerOptions{
		CLI: env("LOOMCYCLE_WORKSPACE_CONTAINER_CLI"), Image: env("LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE"),
		Runtime: env("LOOMCYCLE_WORKSPACE_CONTAINER_RUNTIME"), HostRoot: hostRoot,
		MemoryMB: int64(mem), CPUs: cpus, Pids: int64(pids), TmpMB: int64(tmp), Egress: eg,
	}, nil
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
