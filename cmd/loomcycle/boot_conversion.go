package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/local"
	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
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
	h := conversionHelpers(b.cfg)
	d := dispatch.New(dispatch.Options{
		Token:         token,
		BuildVersion:  buildVersion,
		QueueWait:     time.Duration(cv.QueueWaitS) * time.Second,
		QuarantineTTL: time.Duration(cv.QuarantineTTLS) * time.Second,
		KindTimeouts:  h.timeouts,
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
		Rasterize: h.rasterize, Media: h.media, Encode: h.encode, Wrapper: b.cfg.Env.HelperWrapper,
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

// helperSettings are the operator's helper command lines a local worker runs
// and the time bound of each helper's jobs.
type helperSettings struct {
	rasterize, media, encode string
	timeouts                 map[workerpb.JobKind]time.Duration
}

// conversionHelpers reads the helper settings from the transcription and
// speech blocks: a job runs under the same bound in a worker as it would in
// this process.
func conversionHelpers(cfg *config.Config) helperSettings {
	h := helperSettings{timeouts: map[workerpb.JobKind]time.Duration{
		workerpb.JobKind_JOB_KIND_PDF_RENDER: convert.RasterizeTimeout,
	}}
	t := cfg.Transcription.Resolved()
	if t.OCR != nil && t.OCR.Rasterize != nil {
		h.rasterize = t.OCR.Rasterize.Command
	}
	if t.STT != nil {
		if t.STT.Media != nil {
			h.media = t.STT.Media.Command
		}
		if t.STT.TimeoutSeconds > 0 {
			h.timeouts[workerpb.JobKind_JOB_KIND_MEDIA_CONVERT] = time.Duration(t.STT.TimeoutSeconds) * time.Second
		}
	}
	if sp := cfg.Speech.Resolved(); sp != nil {
		if sp.Encode != nil {
			h.encode = sp.Encode.Command
		}
		if sp.TimeoutSeconds > 0 {
			h.timeouts[workerpb.JobKind_JOB_KIND_AUDIO_ENCODE] = time.Duration(sp.TimeoutSeconds) * time.Second
		}
	}
	return h
}
