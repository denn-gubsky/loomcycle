package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/local"
	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// minWorkerToken is the shortest LOOMCYCLE_WORKER_TOKEN a worker endpoint on
// the network accepts: a token that can be guessed lets anyone who reaches
// the port read every file sent for conversion.
const minWorkerToken = 32

// startConversion starts the conversion worker pool when conversion.mode asks
// for one (RFC GO) and returns the dispatcher every conversion goes through;
// nil in mode inprocess, where nothing is started and every conversion runs
// in this process as before.
//
// Mode local: a Unix socket in a private directory, and conversion.local.workers
// children of this binary that attach to it. Mode remote: a TCP endpoint on
// conversion.listen that workers of their own attach to with
// LOOMCYCLE_WORKER_TOKEN. local+remote: both, one pool. The children are
// stopped, and the listeners closed, at shutdown.
func (b *boot) startConversion() *dispatch.Dispatcher {
	cv := b.cfg.Conversion.Resolved()
	if !cv.LocalWorkers() && !cv.RemoteWorkers() {
		return nil
	}
	token, err := conversionToken(cv)
	if err != nil {
		fatal("conversion: "+err.Error(), "mode", cv.Mode)
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
	serve := func(l net.Listener) {
		safego.Go(context.Background(), "conversion.dispatcher", func() {
			if err := d.Serve(l); err != nil {
				slog.Warn("conversion dispatcher stopped", "err", err, "addr", l.Addr().String())
			}
		})
	}
	// closers end the listeners after the dispatcher stops; stopChildren
	// runs before it, so no child is mid-job when its streams end.
	var (
		closers      []func()
		stopChildren = func() {}
	)
	if cv.RemoteWorkers() {
		l, err := net.Listen("tcp", cv.Listen)
		if err != nil {
			fatal("conversion: cannot open the worker endpoint", "listen", cv.Listen, "err", err)
		}
		if cv.TLS.Enabled() {
			tc, err := dispatch.ServerTLS(cv.TLS.CertFile, cv.TLS.KeyFile, cv.TLS.ClientCAFile)
			if err != nil {
				fatal("conversion.tls: "+err.Error(), "listen", cv.Listen)
			}
			l = dispatch.TLSListener(l, tc)
		}
		serve(l)
		closers = append(closers, func() { l.Close() })
		slog.Info("conversion worker endpoint listening", "listen", cv.Listen, "tls", cv.TLS.Enabled(), "client_certs", cv.TLS.ClientCAFile != "")
	}
	if cv.LocalWorkers() {
		bin, err := os.Executable()
		if err != nil {
			fatal("conversion: cannot find this binary to start workers from", "err", err)
		}
		l, socket, closeSocket, err := local.Listen()
		if err != nil {
			fatal("conversion: cannot open the worker socket", "err", err)
		}
		serve(l)
		sup := local.Start(local.Options{
			Binary: bin, Socket: socket, Token: token,
			Workers: cv.Local.Workers, MemoryLimitMB: cv.Local.MemoryLimitMB,
			Rasterize: h.rasterize, Media: h.media, Encode: h.encode, Wrapper: b.cfg.Env.HelperWrapper,
			OnRespawn: d.AddRespawn, Logger: slog.Default(),
		})
		stopChildren = sup.Stop
		closers = append(closers, closeSocket)
		slog.Info("conversion workers started", "workers", cv.Local.Workers, "memory_limit_mb", cv.Local.MemoryLimitMB)
	}
	b.addCleanup(func() {
		stopChildren()
		d.Stop()
		for _, c := range closers {
			c()
		}
	})
	slog.Info("conversion pool started", "mode", cv.Mode)
	return d
}

// conversionToken is the token workers present. A worker endpoint on the
// network takes the operator's LOOMCYCLE_WORKER_TOKEN and refuses to start
// without one of at least minWorkerToken characters (RFC GO §5.9); the local
// children then present it too. Local alone makes one for this boot.
func conversionToken(cv config.ConversionConfig) (string, error) {
	if !cv.RemoteWorkers() {
		return local.NewToken()
	}
	token := strings.TrimSpace(os.Getenv("LOOMCYCLE_WORKER_TOKEN"))
	if len(token) < minWorkerToken {
		return "", fmt.Errorf("mode %s listens on the network and needs LOOMCYCLE_WORKER_TOKEN of at least %d characters (for example: openssl rand -hex 32)", cv.Mode, minWorkerToken)
	}
	return token, nil
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
