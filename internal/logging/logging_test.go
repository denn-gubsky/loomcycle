package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// stdlibPrefix is the `2006/01/02 15:04:05 ` timestamp the stdlib log
// package writes with its default flags.
var stdlibPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

func newBuf(t *testing.T, opts Options) (*slog.Logger, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, warn bytes.Buffer
	opts.Out, opts.Warn = &out, &warn
	return New(opts), &out, &warn
}

// installForTest runs Setup and restores the slog default plus the stdlib
// log package's writer and flags afterwards (slog.SetDefault rewires both).
func installForTest(t *testing.T, opts Options) *bytes.Buffer {
	t.Helper()
	prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	var out bytes.Buffer
	opts.Out, opts.Warn = &out, &out
	Setup(opts)
	return &out
}

func TestNew_DefaultIsTextAtInfoWithStdlibTimestamp(t *testing.T) {
	l, out, warn := newBuf(t, Options{})
	l.Debug("hidden")
	l.Info("listening", "addr", "127.0.0.1:1")
	l.Warn("disk low", "free", 3)
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != 2 {
		t.Fatalf("want 2 lines (debug filtered), got %q", out.String())
	}
	for _, line := range got {
		if !stdlibPrefix.MatchString(line) {
			t.Errorf("line lacks the stdlib timestamp prefix: %q", line)
		}
	}
	if want := "listening addr=127.0.0.1:1"; stdlibPrefix.ReplaceAllString(got[0], "") != want {
		t.Errorf("info line = %q, want %q (no level tag at INFO)", got[0], want)
	}
	if want := "WARN disk low free=3"; stdlibPrefix.ReplaceAllString(got[1], "") != want {
		t.Errorf("warn line = %q, want %q", got[1], want)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning for the defaults: %q", warn.String())
	}
}

func TestNew_JSONFormatEmitsOneObjectPerLine(t *testing.T) {
	l, out, _ := newBuf(t, Options{Format: "JSON", Level: "debug"})
	l.Debug("probe", "n", 1)
	var rec map[string]any
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("not one JSON object: %v: %q", err, out.String())
	}
	if rec["level"] != "DEBUG" || rec["msg"] != "probe" || rec["n"] != float64(1) {
		t.Errorf("record = %v", rec)
	}
}

func TestNew_LevelSelectionFilters(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  []string
	}{
		{"debug", []string{"d", "i", "w", "e"}},
		{"INFO", []string{"i", "w", "e"}},
		{"warn", []string{"w", "e"}},
		{"error", []string{"e"}},
	} {
		l, out, _ := newBuf(t, Options{Format: "json", Level: tc.level})
		l.Debug("d")
		l.Info("i")
		l.Warn("w")
		l.Error("e")
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("%s: %v", tc.level, err)
			}
			got = append(got, rec["msg"].(string))
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("level %s: emitted %v, want %v", tc.level, got, tc.want)
		}
	}
}

func TestNew_InvalidValuesWarnAndFallBackToDefaults(t *testing.T) {
	l, out, warn := newBuf(t, Options{Format: "yaml", Level: "loud"})
	l.Debug("hidden")
	l.Info("shown")
	if !strings.Contains(warn.String(), EnvFormat+`="yaml"`) || !strings.Contains(warn.String(), EnvLevel+`="loud"`) {
		t.Errorf("warning does not name both bad values: %q", warn.String())
	}
	if got := stdlibPrefix.ReplaceAllString(strings.TrimSpace(out.String()), ""); got != "shown" {
		t.Errorf("fallback should be text at info; got %q", out.String())
	}
}

func TestSetup_BridgesStdlibLogUnchangedInTextMode(t *testing.T) {
	out := installForTest(t, Options{})
	log.Printf("coord: replica upsert failed: %v (will retry in %s)", errors.New("boom"), "5s")
	line := strings.TrimSuffix(out.String(), "\n")
	if strings.Count(out.String(), "\n") != 1 || !stdlibPrefix.MatchString(line) {
		t.Fatalf("bridged line not one stdlib-shaped line: %q", out.String())
	}
	if got, want := stdlibPrefix.ReplaceAllString(line, ""), "coord: replica upsert failed: boom (will retry in 5s)"; got != want {
		t.Errorf("bridged text = %q, want the message unchanged %q", got, want)
	}
}

func TestSetup_BridgesStdlibLogIntoJSONAtInfo(t *testing.T) {
	out := installForTest(t, Options{Format: "json"})
	log.Printf("legacy %d", 7)
	var rec map[string]any
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("bridged line is not JSON: %v: %q", err, out.String())
	}
	if rec["msg"] != "legacy 7" || rec["level"] != "INFO" {
		t.Errorf("record = %v", rec)
	}
}

func TestSetup_BridgedLinesRespectTheLevel(t *testing.T) {
	out := installForTest(t, Options{Level: "warn"})
	log.Printf("an info-level legacy line")
	slog.Warn("kept")
	if strings.Contains(out.String(), "legacy") || !strings.Contains(out.String(), "WARN kept") {
		t.Errorf("at warn the bridged INFO line must drop and the warn stay: %q", out.String())
	}
}

func TestHandler_ContextCallAddsRunIdentityAndSpan(t *testing.T) {
	type key struct{}
	l, out, _ := newBuf(t, Options{Format: "json", Identity: func(ctx context.Context) Identity {
		id, _ := ctx.Value(key{}).(Identity)
		return id
	}})
	ctx := context.WithValue(context.Background(), key{}, Identity{RunID: "run-1", Tenant: "acme", Agent: "writer"})
	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))

	l.InfoContext(ctx, "step")
	var rec map[string]any
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"run_id": "run-1", "tenant": "acme", "agent": "writer",
		"trace_id": tid.String(), "span_id": sid.String(),
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %q", k, rec[k], want)
		}
	}
	if _, ok := rec["user"]; ok {
		t.Errorf("an empty identity field must be omitted: %v", rec)
	}

	out.Reset()
	l.Info("no ctx")
	if strings.Contains(out.String(), "run_id") || strings.Contains(out.String(), "trace_id") {
		t.Errorf("a non-context call must carry no run attributes: %q", out.String())
	}
}

func TestHandler_WithAttrsKeepsContextAttributes(t *testing.T) {
	l, out, _ := newBuf(t, Options{Identity: func(context.Context) Identity { return Identity{RunID: "r9"} }})
	l.With("component", "coord").InfoContext(context.Background(), "tick")
	if got := stdlibPrefix.ReplaceAllString(strings.TrimSpace(out.String()), ""); got != "tick component=coord run_id=r9" {
		t.Errorf("got %q", got)
	}
}

func TestSecret_RendersRedactedInSlogAndFmt(t *testing.T) {
	const raw = "s3cr3t-value-123"
	for _, format := range []string{"text", "json"} {
		l, out, _ := newBuf(t, Options{Format: format})
		l.Info("auth", "bearer", Secret(raw))
		if strings.Contains(out.String(), raw) || !strings.Contains(out.String(), redactedMarker) {
			t.Errorf("%s: secret leaked or not marked: %q", format, out.String())
		}
	}
	if s := fmt.Sprintf("%s %v %q", Secret(raw), Secret(raw), Secret(raw)); strings.Contains(s, raw) {
		t.Errorf("fmt leaked the secret: %q", s)
	}
}

func TestRedactor_ScrubsMessagesStringsAndErrors(t *testing.T) {
	const key = "abcdefgh-not-a-real-key"
	r := redact.New(map[string]string{"PROVIDER_API_KEY": key}, false)
	for _, format := range []string{"text", "json"} {
		l, out, _ := newBuf(t, Options{Format: format, Redactor: r})
		l.Warn("calling with "+key, "header", "Bearer "+key, "err", fmt.Errorf("401 for %s", key))
		if strings.Contains(out.String(), key) {
			t.Errorf("%s: registered secret leaked: %q", format, out.String())
		}
		if got := strings.Count(out.String(), "[redacted:PROVIDER_API_KEY]"); got != 3 {
			t.Errorf("%s: want 3 masked occurrences, got %d: %q", format, got, out.String())
		}
	}
}

func TestSetup_RedactsBridgedStdlibLines(t *testing.T) {
	const key = "zyxwvuts-not-a-real-token"
	out := installForTest(t, Options{Redactor: redact.New(map[string]string{"X_TOKEN": key}, false)})
	log.Printf("legacy line with %s inside", key)
	if strings.Contains(out.String(), key) || !strings.Contains(out.String(), "[redacted:X_TOKEN]") {
		t.Errorf("bridged line not scrubbed: %q", out.String())
	}
}
