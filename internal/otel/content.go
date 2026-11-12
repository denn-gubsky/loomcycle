package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	genai "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// Content capture: a tenant that opted in (and whose operator allows it) gets
// the prompts, the model's answers and the tool calls of its own runs on the
// spans its own destination receives. Nothing else does.
//
// Content is recorded on the span only while ctx carries a ContentCapture,
// which the run's starter sets after its own checks (the operator's gate, the
// tenant's policy, the agent's opt-out, a no-trace run). Every value passes
// the run's redactor first and is then cut to the policy's length. The span is
// shared with the operator's pipeline, whose exporter strips every key in
// ContentAttributeKeys before export (contentStripExporter), so the operator
// never receives what the tenant does.

// ContentAttributeKeys is every attribute key content capture writes, at the
// GenAI semantic-conventions release GenAISemconvVersion pins. It is the one
// list: the recorders below write only these keys, the operator's exporter
// strips exactly these, and the tenant copy's allow-list admits them
// (TestContentAttributeKeys_OneListForCaptureStripAndTenantCopy).
var ContentAttributeKeys = [...]attribute.Key{
	genai.GenAISystemInstructionsKey,
	genai.GenAIInputMessagesKey,
	genai.GenAIOutputMessagesKey,
	genai.GenAIToolCallArgumentsKey,
	genai.GenAIToolCallResultKey,
}

// The bounds of a captured field's length, in characters (runes).
const (
	DefaultContentMaxChars = 4096
	MinContentMaxChars     = 256
	MaxContentMaxChars     = 65536
)

func isContentKey(k attribute.Key) bool {
	for _, c := range ContentAttributeKeys {
		if k == c {
			return true
		}
	}
	return false
}

// ContentCapture is a run's capture: how long a field may be, and the
// redaction every value passes first. Both redactors are required; a capture
// missing one records nothing.
type ContentCapture struct {
	// Tenant is whose content this capture is for: content is recorded only
	// on spans of this tenant (AttrTenant on ctx), so a span opened below for
	// another tenant's work never carries it.
	Tenant string
	// MaxChars cuts each captured field (one attribute) to this many
	// characters, after redaction.
	MaxChars int
	// Redact masks a text value: a prompt, an answer, a tool's result.
	Redact func(string) string
	// RedactArgs masks a tool call's JSON arguments, given the tool's name
	// (some tools' arguments are secrets by field, whatever their value).
	// It returns JSON, or text when the arguments were not JSON.
	RedactArgs func(tool string, args []byte) []byte
	// Active, when set, is asked before each recording: whether the tenant
	// still captures content. A run decides to capture at its start; a tenant
	// that turns capture off mid-run stops it at the next recording. It must
	// be cheap and must not block.
	Active func() bool
}

func (c *ContentCapture) usable() bool {
	return c != nil && c.Tenant != "" && c.MaxChars > 0 && c.Redact != nil && c.RedactArgs != nil
}

type contentCaptureKey struct{}

// WithContentCapture returns ctx whose runs' spans capture content as c says.
// A nil c clears a capture set further up: a sub-run decides for itself (its
// own tenant, its own agent) and must not inherit its parent's.
func WithContentCapture(ctx context.Context, c *ContentCapture) context.Context {
	if !c.usable() {
		if ctx.Value(contentCaptureKey{}) == nil {
			return ctx
		}
		c = nil
	}
	return context.WithValue(ctx, contentCaptureKey{}, c)
}

// ContentCaptureFrom returns ctx's capture, nil when none. Never for the work
// of a no-trace run, never for spans of another tenant than the capture's,
// and never while no tenant router is installed: there is then no
// destination the content could be meant for.
func ContentCaptureFrom(ctx context.Context) *ContentCapture {
	c, _ := ctx.Value(contentCaptureKey{}).(*ContentCapture)
	if !c.usable() || NoTraceFromContext(ctx) || !tenantRouting.Load() || TenantFromContext(ctx) != c.Tenant {
		return nil
	}
	if c.Active != nil && !c.Active() {
		return nil
	}
	return c
}

// ContentPart is one part of a captured message. Text parts carry Text; a
// tool call carries ID, Name and Arguments; a tool's answer ID and Response.
// A binary part (an image, a document) names its MediaType and Bytes only:
// its data is never handed here, and it is recorded as a descriptor.
type ContentPart struct {
	Type      string
	Text      string
	ID        string
	Name      string
	Arguments []byte
	Response  string
	MediaType string
	Bytes     int
}

// The part types of the pinned release this package writes. A binary part
// keeps its own type ("image", "document").
const (
	ContentPartText             = "text"
	ContentPartToolCall         = "tool_call"
	ContentPartToolCallResponse = "tool_call_response"
)

// ContentMessage is one captured message.
type ContentMessage struct {
	Role         string
	Parts        []ContentPart
	FinishReason string
}

// The JSON shapes of the pinned release's gen_ai.*.messages attributes, plus
// the binary descriptor RFC GG defines ({type, media_type, bytes}).
type semconvPart struct {
	Type      string          `json:"type"`
	Content   *string         `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  *string         `json:"response,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Bytes     *int            `json:"bytes,omitempty"`
}

type semconvMessage struct {
	Role         string        `json:"role"`
	Parts        []semconvPart `json:"parts"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

// part is p as recorded: redacted, and a binary part reduced to its
// descriptor.
func (c *ContentCapture) part(p ContentPart) semconvPart {
	if p.MediaType != "" {
		n := max(p.Bytes, 0)
		return semconvPart{Type: p.Type, MediaType: p.MediaType, Bytes: &n}
	}
	out := semconvPart{Type: p.Type, ID: p.ID, Name: p.Name}
	switch p.Type {
	case ContentPartToolCall:
		if len(p.Arguments) > 0 {
			out.Arguments = c.args(p.Name, p.Arguments)
		}
	case ContentPartToolCallResponse:
		r := c.Redact(p.Response)
		out.Response = &r
	default:
		t := c.Redact(p.Text)
		out.Content = &t
	}
	return out
}

// args is a tool call's arguments as a JSON value: redacted JSON, or the
// redacted text as a JSON string when the arguments were not JSON.
func (c *ContentCapture) args(tool string, raw []byte) json.RawMessage {
	red := c.RedactArgs(tool, raw)
	if json.Valid(red) {
		return red
	}
	b, _ := json.Marshal(string(red))
	return b
}

func (c *ContentCapture) parts(ps []ContentPart) []semconvPart {
	out := make([]semconvPart, 0, len(ps))
	for _, p := range ps {
		out = append(out, c.part(p))
	}
	return out
}

func (c *ContentCapture) messages(ms []ContentMessage) []semconvMessage {
	out := make([]semconvMessage, 0, len(ms))
	for _, m := range ms {
		out = append(out, semconvMessage{Role: m.Role, Parts: c.parts(m.Parts), FinishReason: m.FinishReason})
	}
	return out
}

// field is v serialised and cut to MaxChars. Redaction happened on each value
// before it was serialised: a secret is matched as it was, not as JSON
// escaped it, and a cut can never leave the head of a secret behind.
func (c *ContentCapture) field(k attribute.Key, v any) attribute.KeyValue {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return k.String(TruncateContent(string(bytes.TrimRight(buf.Bytes(), "\n")), c.MaxChars))
}

// TruncateContent cuts s to max characters (runes), marking what was cut:
// "…[truncated N chars]". A value within max is returned as is.
func TruncateContent(s string, max int) string {
	n := utf8.RuneCountInString(s)
	if max <= 0 || n <= max {
		return s
	}
	i, seen := 0, 0
	for i < len(s) && seen < max {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		seen++
	}
	return s[:i] + "…[truncated " + strconv.Itoa(n-max) + " chars]"
}

// SetModelInputContent records a model call's system instructions and input
// messages on span, when ctx captures content.
func SetModelInputContent(ctx context.Context, span trace.Span, system []ContentPart, input []ContentMessage) {
	c := ContentCaptureFrom(ctx)
	if c == nil || span == nil || !span.IsRecording() {
		return
	}
	kvs := make([]attribute.KeyValue, 0, 2)
	if len(system) > 0 {
		kvs = append(kvs, c.field(genai.GenAISystemInstructionsKey, c.parts(system)))
	}
	kvs = append(kvs, c.field(genai.GenAIInputMessagesKey, c.messages(input)))
	span.SetAttributes(kvs...)
}

// SetModelOutputContent records a model call's output messages on span, when
// ctx captures content.
func SetModelOutputContent(ctx context.Context, span trace.Span, output []ContentMessage) {
	c := ContentCaptureFrom(ctx)
	if c == nil || span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(c.field(genai.GenAIOutputMessagesKey, c.messages(output)))
}

// SetToolCallContent records a tool call's arguments and result on span, when
// ctx captures content.
func SetToolCallContent(ctx context.Context, span trace.Span, tool string, args []byte, result string) {
	c := ContentCaptureFrom(ctx)
	if c == nil || span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(
		genai.GenAIToolCallArgumentsKey.String(TruncateContent(string(c.args(tool, args)), c.MaxChars)),
		genai.GenAIToolCallResultKey.String(TruncateContent(c.Redact(result), c.MaxChars)),
	)
}

// contentStripExporter is in front of the operator's exporter: every span it
// exports goes without the content keys, on the span and on its events. The
// span itself, shared with the tenant's pipeline, is never modified.
type contentStripExporter struct {
	next sdktrace.SpanExporter
}

func (e *contentStripExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := spans
	copied := false
	for i, s := range spans {
		if !carriesContent(s) {
			continue
		}
		if !copied {
			out = append([]sdktrace.ReadOnlySpan(nil), spans...)
			copied = true
		}
		out[i] = newStrippedSpan(s)
	}
	return e.next.ExportSpans(ctx, out)
}

func (e *contentStripExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

func carriesContent(s sdktrace.ReadOnlySpan) bool {
	if hasContentKey(s.Attributes()) {
		return true
	}
	for _, ev := range s.Events() {
		if hasContentKey(ev.Attributes) {
			return true
		}
	}
	return false
}

func hasContentKey(kvs []attribute.KeyValue) bool {
	for _, kv := range kvs {
		if isContentKey(kv.Key) {
			return true
		}
	}
	return false
}

func withoutContent(kvs []attribute.KeyValue) []attribute.KeyValue {
	if !hasContentKey(kvs) {
		return kvs
	}
	out := make([]attribute.KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		if !isContentKey(kv.Key) {
			out = append(out, kv)
		}
	}
	return out
}

// strippedSpan is s without the content keys.
type strippedSpan struct {
	sdktrace.ReadOnlySpan
	attrs  []attribute.KeyValue
	events []sdktrace.Event
}

func newStrippedSpan(s sdktrace.ReadOnlySpan) strippedSpan {
	out := strippedSpan{ReadOnlySpan: s, attrs: withoutContent(s.Attributes())}
	for _, ev := range s.Events() {
		ev.Attributes = withoutContent(ev.Attributes)
		out.events = append(out.events, ev)
	}
	return out
}

func (s strippedSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s strippedSpan) Events() []sdktrace.Event         { return s.events }
