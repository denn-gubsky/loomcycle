// Package logging is loomcycle's structured-logging setup on the standard
// library's log/slog. It picks the output format and level from the process
// env, installs the result as the slog default, and — through
// slog.SetDefault's own bridge — routes every un-migrated log.Printf through
// the same handler, so a deployment gets ONE format whichever API a call
// site uses.
//
// The default (text) format is deliberately not slog.TextHandler's
// `time=… level=… msg=…`: an operator's terminal and grep habits are built on
// the stdlib log line, so text mode keeps `2006/01/02 15:04:05 <message>`
// and appends structured attributes as key=value after it. A bridged
// log.Printf line therefore reads exactly as it did before slog.
package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// Env var names read by the wiring in cmd/loomcycle.
const (
	EnvFormat = "LOOMCYCLE_LOG_FORMAT"
	EnvLevel  = "LOOMCYCLE_LOG_LEVEL"
)

// Identity is the run identity a ctx-aware log call is tagged with. Empty
// fields are omitted from the record.
type Identity struct {
	RunID  string
	Tenant string
	Agent  string
	User   string
}

// Options configures Setup. The zero value is text at INFO to stderr.
type Options struct {
	// Format is the raw LOOMCYCLE_LOG_FORMAT value: "text" (default) or
	// "json". Anything else warns and falls back to text.
	Format string
	// Level is the raw LOOMCYCLE_LOG_LEVEL value: debug | info (default) |
	// warn | error, case-insensitive. Anything else warns and falls back to
	// info.
	Level string
	// Out receives log lines; nil means os.Stderr. Never stdout: in
	// `loomcycle mcp` mode stdout is the JSON-RPC wire.
	Out io.Writer
	// Warn receives the one-line warning for an invalid Format/Level; nil
	// means os.Stderr.
	Warn io.Writer
	// Redactor masks known secret values in every logged message, string
	// attribute and error attribute. nil disables scrubbing (the Secret type
	// still renders as [redacted]).
	Redactor *redact.Redactor
	// Identity extracts the run identity from a ctx. It is injected rather
	// than imported because the accessors live in internal/tools, which sits
	// above most of the packages that log — importing it here would make
	// those packages unable to import logging. nil means no run attributes.
	Identity func(context.Context) Identity
}

// Secret is a string that never reaches a log line: slog renders it as
// [redacted], and so does fmt (%s/%v/%q) through String.
type Secret string

const redactedMarker = "[redacted]"

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redactedMarker) }

// String implements fmt.Stringer so a Secret passed to a Printf is masked too.
func (Secret) String() string { return redactedMarker }

// Setup builds the logger described by opts, installs it as the slog default
// (which also redirects the stdlib log package into it, at INFO), and
// returns it.
func Setup(opts Options) *slog.Logger {
	l := New(opts)
	slog.SetDefault(l)
	return l
}

// New builds the logger described by opts without installing it.
func New(opts Options) *slog.Logger {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}
	warn := opts.Warn
	if warn == nil {
		warn = os.Stderr
	}
	format, ok := parseFormat(opts.Format)
	if !ok {
		fmt.Fprintf(warn, "loomcycle: %s=%q is not text|json; using text\n", EnvFormat, opts.Format)
	}
	level, ok := parseLevel(opts.Level)
	if !ok {
		fmt.Fprintf(warn, "loomcycle: %s=%q is not debug|info|warn|error; using info\n", EnvLevel, opts.Level)
	}

	scrub := scrubber(opts.Redactor)
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(out, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				return scrub(a)
			},
		})
	} else {
		h = newTextHandler(out, level, scrub)
	}
	return slog.New(&ctxHandler{inner: h, identity: opts.Identity})
}

func parseFormat(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text":
		return "text", true
	case "json":
		return "json", true
	}
	return "text", false
}

func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// scrubber returns the attribute rewrite that masks registered secret values.
// It runs on resolved values (LogValuers already expanded), including the
// message itself, which is how un-migrated log.Printf text gets scrubbed.
// Only strings and errors are inspected: an arbitrary struct is formatted
// later by the handler, past the point a rewrite could see its text.
func scrubber(r *redact.Redactor) func(slog.Attr) slog.Attr {
	return func(a slog.Attr) slog.Attr {
		if !r.Enabled() {
			return a
		}
		switch a.Value.Kind() {
		case slog.KindString:
			if s := a.Value.String(); s != "" {
				if m := r.String(s); m != s {
					a.Value = slog.StringValue(m)
				}
			}
		case slog.KindAny:
			if err, ok := a.Value.Any().(error); ok && err != nil {
				a.Value = slog.StringValue(r.String(err.Error()))
			}
		}
		return a
	}
}

// ctxHandler adds the run identity and the OTel span ids carried on the
// call's ctx. It sees a ctx only on slog.*Context calls; a bridged
// log.Printf arrives with context.Background and gets nothing added.
type ctxHandler struct {
	inner    slog.Handler
	identity func(context.Context) Identity
}

func (h *ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		var attrs []slog.Attr
		if h.identity != nil {
			id := h.identity(ctx)
			attrs = appendNonEmpty(attrs, "run_id", id.RunID)
			attrs = appendNonEmpty(attrs, "tenant", id.Tenant)
			attrs = appendNonEmpty(attrs, "agent", id.Agent)
			attrs = appendNonEmpty(attrs, "user", id.User)
		}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			attrs = append(attrs,
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()))
		}
		if len(attrs) > 0 {
			// Clone: the caller's Record may share its attr backing array.
			r = r.Clone()
			r.AddAttrs(attrs...)
		}
	}
	return h.inner.Handle(ctx, r)
}

func appendNonEmpty(attrs []slog.Attr, key, val string) []slog.Attr {
	if val == "" {
		return attrs
	}
	return append(attrs, slog.String(key, val))
}

func (h *ctxHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ctxHandler{inner: h.inner.WithAttrs(as), identity: h.identity}
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	return &ctxHandler{inner: h.inner.WithGroup(name), identity: h.identity}
}

// textHandler writes `2006/01/02 15:04:05 [LEVEL ]message[ key=value…]`.
// INFO carries no level tag so a bridged log.Printf line is byte-identical
// to the stdlib log output it replaces. The key=value tail is rendered by an
// inner slog.TextHandler (quoting, groups, WithAttrs) into a buffer shared by
// every handler derived from one root; mu serialises the reset-render-read.
type textHandler struct {
	mu    *sync.Mutex
	buf   *bytes.Buffer
	inner slog.Handler
	out   io.Writer
	level slog.Leveler
	scrub func(slog.Attr) slog.Attr
}

func newTextHandler(out io.Writer, level slog.Leveler, scrub func(slog.Attr) slog.Attr) *textHandler {
	buf := &bytes.Buffer{}
	inner := slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug, // the outer handler gates the level
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 {
				switch a.Key {
				case slog.TimeKey, slog.LevelKey, slog.MessageKey:
					return slog.Attr{} // rendered by the outer handler
				}
			}
			return scrub(a)
		},
	})
	return &textHandler{mu: &sync.Mutex{}, buf: buf, inner: inner, out: out, level: level, scrub: scrub}
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *textHandler) Handle(ctx context.Context, r slog.Record) error {
	msg := h.scrub(slog.String(slog.MessageKey, r.Message)).Value.String()
	attrsOnly := slog.NewRecord(r.Time, r.Level, "", r.PC)
	r.Attrs(func(a slog.Attr) bool {
		attrsOnly.AddAttrs(a)
		return true
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Reset()
	if err := h.inner.Handle(ctx, attrsOnly); err != nil {
		return err
	}
	tail := bytes.TrimSpace(h.buf.Bytes())

	line := make([]byte, 0, 32+len(msg)+len(tail))
	if !r.Time.IsZero() {
		line = r.Time.AppendFormat(line, "2006/01/02 15:04:05")
		line = append(line, ' ')
	}
	if r.Level != slog.LevelInfo {
		line = append(line, r.Level.String()...)
		line = append(line, ' ')
	}
	line = append(line, msg...)
	if len(tail) > 0 {
		line = append(line, ' ')
		line = append(line, tail...)
	}
	line = append(line, '\n')
	_, err := h.out.Write(line)
	return err
}

func (h *textHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.inner = h.inner.WithAttrs(as)
	return &c
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.inner = h.inner.WithGroup(name)
	return &c
}
