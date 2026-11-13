package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/local"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// startConversion starts the conversion worker pool when conversion.mode asks
// for one (RFC GO) and returns the client the File tool converts through; nil
// in mode inprocess, where nothing is started and the File tool converts in
// this process as before.
//
// Mode local: a dispatcher on a Unix socket in a private directory, and
// conversion.local.workers children of this binary that attach to it with a
// token made for this boot. The children are stopped, and the socket removed,
// at shutdown.
func (b *boot) startConversion() *dispatch.Dispatcher {
	cv := b.cfg.Conversion.Resolved()
	if cv.Mode != config.ConversionLocal {
		return nil
	}
	bin, err := os.Executable()
	if err != nil {
		fatal("conversion: cannot find this binary to start workers from", "err", err)
	}
	token, err := local.NewToken()
	if err != nil {
		fatal("conversion: cannot make a worker token", "err", err)
	}
	l, socket, closeSocket, err := local.Listen()
	if err != nil {
		fatal("conversion: cannot open the worker socket", "err", err)
	}
	d := dispatch.New(dispatch.Options{
		Token:         token,
		BuildVersion:  buildVersion,
		QueueWait:     time.Duration(cv.QueueWaitS) * time.Second,
		QuarantineTTL: time.Duration(cv.QuarantineTTLS) * time.Second,
		Logger:        slog.Default(),
	})
	safego.Go(context.Background(), "conversion.dispatcher", func() {
		if err := d.Serve(l); err != nil {
			slog.Warn("conversion dispatcher stopped", "err", err)
		}
	})
	sup := local.Start(local.Options{
		Binary: bin, Socket: socket, Token: token,
		Workers: cv.Local.Workers, MemoryLimitMB: cv.Local.MemoryLimitMB,
		OnRespawn: d.AddRespawn, Logger: slog.Default(),
	})
	b.addCleanup(func() {
		sup.Stop()
		d.Stop()
		closeSocket()
	})
	slog.Info("conversion workers started", "mode", cv.Mode, "workers", cv.Local.Workers, "memory_limit_mb", cv.Local.MemoryLimitMB)
	return d
}
