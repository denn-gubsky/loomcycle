package otel

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// TestSDKLogger_AMalformedHeaderValueReachesNoLog is the probe for the SDK's
// header parser: built with a malformed OTEL_EXPORTER_OTLP_HEADERS (the boot
// refuses it, but the exporter must be safe on its own), the exporter reports
// the entry through the SDK logger, and the line that reaches slog carries
// the static message only.
func TestSDKLogger_AMalformedHeaderValueReachesNoLog(t *testing.T) {
	clearOTELEnv(t)
	var logs lineCounter
	otel.SetLogger(newSDKLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	defer otel.SetLogger(newSDKLogger(slog.New(slog.DiscardHandler)))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=SUPERSECRET%zz,SUPERSECRET2,bad key=SUPERSECRET3")
	for _, p := range []Protocol{ProtocolHTTPProtobuf, ProtocolGRPC} {
		exp, err := newExporter(context.Background(), p, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_ = exp.Shutdown(ctx)
		cancel()
	}
	out := strings.Join(logs.lines(), "\n")
	if !strings.Contains(out, "otel sdk:") {
		t.Fatalf("the SDK's header complaints did not reach the sink, so this probe proves nothing:\n%s", out)
	}
	if strings.Contains(out, "SUPERSECRET") {
		t.Errorf("a header value reached the log:\n%s", out)
	}
}

// TestSanitizeError_StripsURLCredentials: the exporter quotes the endpoint in
// a failed request's error; userinfo and query values never reach the log.
func TestSanitizeError_StripsURLCredentials(t *testing.T) {
	err := errors.New(`traces export: Post "https://otel:SUPERSECRET@collector.example:4318/v1/traces?api_key=SUPERSECRET2": dial tcp: refused`)
	got := sanitizeError(err)
	if strings.Contains(got, "SUPERSECRET") {
		t.Errorf("sanitized error still carries a credential: %s", got)
	}
	if !strings.Contains(got, "collector.example:4318/v1/traces") || !strings.Contains(got, "dial tcp: refused") {
		t.Errorf("sanitized error lost its substance: %s", got)
	}

	var logs lineCounter
	h := newErrorHandler(slog.New(slog.NewTextHandler(&logs, nil)), time.Now)
	h.Handle(err)
	if out := strings.Join(logs.lines(), "\n"); strings.Contains(out, "SUPERSECRET") {
		t.Errorf("the error handler logged a credential: %s", out)
	}
}
