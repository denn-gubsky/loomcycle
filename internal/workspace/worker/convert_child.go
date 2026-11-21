package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	cdispatch "github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/local"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// The conversion child (RFC GW D34): a workspace's file is converted next to
// it, on its worker, never in the core. The worker does not parse a file it
// converts itself: it starts one conversion worker as its child, on a
// private socket with a per-boot token, exactly as the core does in
// conversion.mode local (internal/conversion/local), with a memory limit,
// restarted with backoff when it dies and ending when this worker does
// (--parent-pid). A file that kills the child fails that one call; this
// process and its workspaces live on.

// DefaultConvertMemoryLimitMB is the conversion child's memory limit when
// none is set (LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB; config
// workspace.convert.memory_limit_mb in local mode).
const DefaultConvertMemoryLimitMB = 1024

// childQuarantine is the child dispatcher's own quarantine time. The core
// keeps the quarantine per workspace (conversion.quarantine_ttl_s); one kept
// here would refuse the same file to every workspace on this worker, so it
// is as short as the dispatcher allows.
const childQuarantine = time.Nanosecond

// ConvertChild configures the conversion child a worker starts.
type ConvertChild struct {
	// Binary is this loomcycle executable.
	Binary string
	// MemoryLimitMB is the child's GOMEMLIMIT; zero is
	// DefaultConvertMemoryLimitMB.
	MemoryLimitMB int
	// The helper command lines the child runs, as local.Options names them;
	// empty leaves the child its defaults from PATH.
	Rasterize, Media, Encode, WritePDF, Draw, Wrapper string
	DjVuText, DjVuInspect, DjVuRasterize              string
}

// startConverter starts the conversion child and the private dispatcher it
// attaches to, and returns the client the convert ops call and a stop that
// ends both.
func startConverter(build string, c *ConvertChild, log *slog.Logger) (convert.Client, func(), error) {
	if c.Binary == "" {
		return nil, nil, errors.New("no binary to start the conversion child from")
	}
	l, socket, closeSocket, err := local.Listen()
	if err != nil {
		return nil, nil, err
	}
	token, err := local.NewToken()
	if err != nil {
		closeSocket()
		return nil, nil, fmt.Errorf("the conversion child's token: %w", err)
	}
	d := cdispatch.New(cdispatch.Options{Token: token, BuildVersion: build, QuarantineTTL: childQuarantine, Logger: log})
	safego.Go(context.Background(), "workspace.worker.convert-dispatcher", func() {
		if err := d.Serve(l); err != nil {
			log.Warn("the conversion child's dispatcher stopped", "err", err)
		}
	})
	mem := c.MemoryLimitMB
	if mem <= 0 {
		mem = DefaultConvertMemoryLimitMB
	}
	sup := local.Start(local.Options{
		Binary: c.Binary, Socket: socket, Token: token, Workers: 1, MemoryLimitMB: mem,
		Rasterize: c.Rasterize, Media: c.Media, Encode: c.Encode, Wrapper: c.Wrapper, WritePDF: c.WritePDF, Draw: c.Draw,
		DjVuText: c.DjVuText, DjVuInspect: c.DjVuInspect, DjVuRasterize: c.DjVuRasterize,
		OnRespawn: d.AddRespawn, Logger: log,
	})
	log.Info("conversion child started", "memory_limit_mb", mem)
	return d.Client(), func() {
		sup.Stop()
		d.Stop()
		closeSocket()
	}, nil
}
