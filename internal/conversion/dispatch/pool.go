package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// Bounds on what a worker may send back. Its answer is untrusted input; the
// core's own caps (the sink's Markdown cap, the inbox's per-file cap) apply
// on top when it is kept.
const (
	maxImageBytes = 64 << 20
	// maxFreeRetries bounds the retries that are no one's fault: a Busy, a
	// slot whose stream ended before the worker accepted the job.
	maxFreeRetries = 3
)

// job is one conversion handed to a slot.
type job struct {
	ctx       context.Context
	id        string
	kind      workerpb.JobKind
	r         io.ReaderAt
	size      int64
	mediaType string
	name      string
	opts      extract.Options
	pages     []int
	sink      extract.Sink
	done      chan result // buffered 1
}

// result is how a job ended on its slot.
type result struct {
	report extract.Report
	out    []byte
	err    error
	// retry: nothing of the job reached the worker (Busy, or the slot went
	// away first); it may run elsewhere at no cost.
	retry bool
	// died: the worker's stream ended without Done. accepted says whether
	// the worker had taken the job; delivered whether anything reached the
	// caller's sink, after which the job cannot run again.
	died, accepted, delivered bool
}

var errProtocol = errors.New("the worker broke the protocol")

// serve runs j on the slot's stream. Every send happens on this goroutine;
// a second one only receives.
func (d *Dispatcher) serve(ctx context.Context, stream workerpb.WorkerService_WorkServer, s *slot, j *job) (res result) {
	d.mu.Lock()
	d.inFlight++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inFlight--
		d.mu.Unlock()
	}()
	log := d.log.With("job_id", j.id, "worker_id", s.w.id, "kind", wire.KindName(j.kind))

	start := &workerpb.JobStart{JobId: j.id, Kind: j.kind, InputSize: j.size, MediaType: j.mediaType, Name: j.name,
		TimeoutMs: d.opt.JobTimeout.Milliseconds()}
	if j.kind == workerpb.JobKind_JOB_KIND_EXTRACT {
		start.Extract = &workerpb.ExtractOptions{Images: j.opts.Images, MaxRows: wire.I32(j.opts.MaxRows), InferHeadings: j.opts.InferHeadings}
	}
	for _, p := range j.pages {
		start.Pages = append(start.Pages, wire.I32(p))
	}
	if err := stream.Send(&workerpb.JobFrame{Msg: &workerpb.JobFrame_Start{Start: start}}); err != nil {
		return result{retry: true}
	}

	frames := make(chan *workerpb.WorkerFrame)
	recvErr := make(chan error, 1)
	safego.Go(ctx, "conversion.dispatch.recv", func() {
		for {
			f, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	})

	// Accept or Busy, before any of the file is sent.
	acc := time.NewTimer(acceptWait)
	defer acc.Stop()
	select {
	case f := <-frames:
		switch {
		case f.GetBusy() != nil:
			return result{retry: true}
		case f.GetAccept() == nil:
			log.Warn("conversion worker answered a job with neither Accept nor Busy")
			return result{retry: true}
		}
	case <-recvErr:
		return result{retry: true}
	case <-ctx.Done():
		return result{retry: true}
	case <-j.ctx.Done():
		return result{err: j.ctx.Err()}
	case <-acc.C:
		log.Warn("conversion worker did not accept a job in time")
		return result{retry: true}
	}
	res.accepted = true

	// The input, in chunks.
	sr := io.NewSectionReader(j.r, 0, j.size)
	buf := make([]byte, wire.ChunkSize)
	for {
		if err := j.ctx.Err(); err != nil {
			return result{err: err, accepted: true}
		}
		n, rerr := io.ReadFull(sr, buf)
		if n > 0 {
			if err := stream.Send(&workerpb.JobFrame{Msg: &workerpb.JobFrame_InputChunk{InputChunk: &workerpb.Chunk{Data: buf[:n]}}}); err != nil {
				return result{died: true, accepted: true}
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return result{err: fmt.Errorf("reading the file to convert: %w", rerr), accepted: true}
		}
	}
	if err := stream.Send(&workerpb.JobFrame{Msg: &workerpb.JobFrame_InputEnd{InputEnd: &workerpb.InputEnd{}}}); err != nil {
		return result{died: true, accepted: true}
	}

	// The output, until Done.
	deadline := time.NewTimer(d.opt.JobTimeout + deadlineGrace)
	defer deadline.Stop()
	var (
		sinkFull, cancelled bool
		sinkErr             error
		img                 *extract.Image
		imgSize             int64
		out                 []byte
	)
	maxOut := 2*j.size + wire.ChunkSize
	cancel := func(reason workerpb.CancelReason) bool {
		if cancelled {
			return true
		}
		cancelled = true
		return stream.Send(&workerpb.JobFrame{Msg: &workerpb.JobFrame_Cancel{Cancel: &workerpb.Cancel{Reason: reason}}}) == nil
	}
	broke := func(what string) result {
		log.Warn("conversion worker broke the protocol", "what", what)
		return result{died: true, accepted: true, delivered: res.delivered, err: errProtocol}
	}
	for {
		select {
		case f := <-frames:
			switch m := f.GetMsg().(type) {
			case *workerpb.WorkerFrame_Text:
				if j.sink == nil {
					return broke("text on a job with no sink")
				}
				if sinkFull || sinkErr != nil {
					continue
				}
				res.delivered = true
				if err := j.sink.Text(m.Text.GetMarkdown()); err != nil {
					if errors.Is(err, extract.ErrSinkFull) {
						sinkFull = true
						if !cancel(workerpb.CancelReason_CANCEL_REASON_SINK_FULL) {
							return result{died: true, accepted: true, delivered: true}
						}
					} else {
						sinkErr = err
						if !cancel(workerpb.CancelReason_CANCEL_REASON_CALLER) {
							return result{died: true, accepted: true, delivered: true}
						}
					}
				}
			case *workerpb.WorkerFrame_PageStart:
				if ps, ok := j.sink.(extract.PageSink); ok && !sinkFull && sinkErr == nil {
					ps.StartPage(int(m.PageStart.GetPage()))
				}
			case *workerpb.WorkerFrame_ImageBegin:
				b := m.ImageBegin
				if img != nil || j.sink == nil || b.GetSize() < 0 || b.GetSize() > maxImageBytes {
					return broke("image_begin")
				}
				img = &extract.Image{Name: b.GetName(), MediaType: b.GetMediaType(), Alt: b.GetAlt(),
					Page: int(b.GetPage()), Slide: int(b.GetSlide()), Data: make([]byte, 0, b.GetSize())}
				imgSize = b.GetSize()
			case *workerpb.WorkerFrame_ImageChunk:
				if img == nil || int64(len(img.Data)+len(m.ImageChunk.GetData())) > imgSize {
					return broke("image_chunk")
				}
				img.Data = append(img.Data, m.ImageChunk.GetData()...)
			case *workerpb.WorkerFrame_ImageEnd:
				if img == nil || int64(len(img.Data)) != imgSize {
					return broke("image_end")
				}
				ref := ""
				if !sinkFull && sinkErr == nil {
					res.delivered = true
					r, err := j.sink.Image(*img)
					if err != nil {
						sinkErr = err
						cancel(workerpb.CancelReason_CANCEL_REASON_CALLER)
					}
					ref = r
				}
				img = nil
				if err := stream.Send(&workerpb.JobFrame{Msg: &workerpb.JobFrame_ImageRef{ImageRef: &workerpb.ImageRef{Ref: ref}}}); err != nil {
					return result{died: true, accepted: true, delivered: res.delivered}
				}
			case *workerpb.WorkerFrame_OutputChunk:
				if int64(len(out)+len(m.OutputChunk.GetData())) > maxOut {
					return broke("output over its bound")
				}
				out = append(out, m.OutputChunk.GetData()...)
			case *workerpb.WorkerFrame_Done:
				err := wire.ErrorFromWire(m.Done.GetErrorClass(), m.Done.GetMessage())
				switch {
				case sinkErr != nil:
					err = sinkErr
				case sinkFull && (err == nil || errors.Is(err, context.Canceled)):
					err = extract.ErrSinkFull
				}
				return result{report: wire.ReportFromWire(m.Done.GetReport()), out: out, err: err, accepted: true, delivered: res.delivered}
			default:
				return broke(fmt.Sprintf("unexpected frame %T", m))
			}
		case <-recvErr:
			return result{died: true, accepted: true, delivered: res.delivered}
		case <-ctx.Done():
			return result{died: true, accepted: true, delivered: res.delivered}
		case <-j.ctx.Done():
			// Returning ends the stream; the worker stops the job.
			return result{err: j.ctx.Err(), accepted: true, delivered: res.delivered}
		case <-deadline.C:
			log.Warn("conversion worker ran past the job's time bound")
			return result{err: context.DeadlineExceeded, accepted: true, delivered: res.delivered}
		}
	}
}

// ---- the client ----

// Client is the pool as a convert.Client. It runs extraction and the PDF page
// cut on the workers; the other jobs arrive with RFC GO P2, and until then a
// caller routes them elsewhere (convert.Phased).
func (d *Dispatcher) Client() convert.Client { return poolClient{d} }

type poolClient struct{ d *Dispatcher }

func (p poolClient) Extract(ctx context.Context, mediaType string, r io.ReaderAt, size int64, sink extract.Sink, opts extract.Options) (extract.Report, error) {
	res, err := p.d.run(ctx, &job{kind: workerpb.JobKind_JOB_KIND_EXTRACT, r: r, size: size, mediaType: mediaType, name: opts.Name, opts: opts, sink: sink})
	if err != nil {
		return extract.Report{}, err
	}
	return res.report, res.err
}

func (p poolClient) SelectPages(ctx context.Context, r io.ReaderAt, size int64, pages []int) ([]byte, error) {
	res, err := p.d.run(ctx, &job{kind: workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES, r: r, size: size, mediaType: "application/pdf", pages: pages})
	if err != nil {
		return nil, err
	}
	if res.err != nil {
		return nil, res.err
	}
	return res.out, nil
}

func (poolClient) RenderPage(context.Context, []byte, string) ([]byte, error) {
	return nil, convert.ErrNotConfigured
}

func (poolClient) ConvertMedia(context.Context, io.ReaderAt, int64, string, string) (convert.Spool, error) {
	return nil, convert.ErrNotConfigured
}

func (poolClient) EncodeAudio(context.Context, []byte, string) ([]byte, error) {
	return nil, convert.ErrNotConfigured
}

func (poolClient) ReencodeImage(context.Context, []byte) ([]byte, string, error) {
	return nil, "", convert.ErrNotConfigured
}

// run hands j to a slot and follows it to its end: retried at no cost when
// nothing reached a worker, quarantined when it killed one (a multi-slot
// worker's death is ambiguous, so its job is retried once first, if nothing
// of it reached the caller yet).
func (d *Dispatcher) run(ctx context.Context, j *job) (result, error) {
	kind := wire.KindName(j.kind)
	digest, err := digestOf(ctx, j.r, j.size)
	if err != nil {
		return result{}, err
	}
	key := kind + "|" + digest
	if d.quarantined(key) {
		d.count(kind, "quarantined", false)
		return result{}, convert.ErrQuarantined
	}
	j.ctx = ctx
	j.id = strconv.FormatUint(d.jobs.Add(1), 10)
	label := wire.LabelFor(j.kind)
	free, ambiguous := maxFreeRetries, 1
	for {
		s, err := d.acquire(ctx, label)
		if err != nil {
			switch {
			case errors.Is(err, convert.ErrNoWorkers):
				d.count(kind, "no_worker", false)
			case ctx.Err() == nil:
				d.count(kind, "busy", false)
			}
			return result{}, err
		}
		j.done = make(chan result, 1)
		s.jobs <- j
		var res result
		select {
		case res = <-j.done:
		case <-ctx.Done():
			// The slot's handler sees the same context and ends the job.
			return result{}, ctx.Err()
		}
		switch {
		case res.retry || (res.died && !res.accepted):
			if free == 0 {
				d.count(kind, "busy", false)
				return result{}, errNoSlot
			}
			free--
			continue
		case res.died:
			if s.w.slots > 1 && ambiguous > 0 && !res.delivered {
				ambiguous--
				d.count(kind, "retried_after_death", true)
				continue
			}
			d.quarantineFor(key)
			d.count(kind, "crashed", true)
			d.log.Warn("a conversion killed its worker; the file is quarantined", "job_id", j.id, "worker_id", s.w.id, "kind", kind, "for", d.opt.QuarantineTTL)
			return result{}, convert.ErrQuarantined
		}
		outcome := "ok"
		if res.err != nil {
			outcome = "error"
		}
		d.count(kind, outcome, false)
		return res, nil
	}
}

// digestOf is the input's SHA-256, the quarantine key: one more read of a
// bounded local file, made before anything is sent, so a quarantined file
// never reaches a worker.
func digestOf(ctx context.Context, r io.ReaderAt, size int64) (string, error) {
	h := sha256.New()
	sr := io.NewSectionReader(r, 0, size)
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := sr.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading the file to convert: %w", err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
