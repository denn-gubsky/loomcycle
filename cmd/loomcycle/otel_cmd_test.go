package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The CLI exists so an investigation can find one user's traces: what it
// prints must be exactly what that user's run span carries under the same key.
func TestOtelUserHash_PrintsTheHashTheRunSpanCarries(t *testing.T) {
	t.Setenv("LOOMCYCLE_SECRET_KEY", testSecretKey())
	var out, errOut bytes.Buffer
	if code := runOtel([]string{"user-hash", "acme", "alice"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	printed := strings.TrimSpace(out.String())

	key, err := credential.DeriveOTelUserKey(testSecretKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lcotel.SetUserIDPolicyForTest(key, false))
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(lcotel.SetTracerProviderForTest(tp))
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1", Tenant: "acme", UserID: "alice"})
	span.End()

	var onSpan string
	for _, kv := range exp.GetSpans()[0].Attributes {
		switch string(kv.Key) {
		case lcotel.AttrUserHash:
			onSpan = kv.Value.AsString()
		case lcotel.AttrUserID:
			t.Errorf("the run span carries the raw %s", lcotel.AttrUserID)
		}
	}
	if printed == "" || printed != onSpan {
		t.Fatalf("CLI printed %q, the run span carries %q", printed, onSpan)
	}

	out.Reset()
	if code := runOtel([]string{"user-hash", "beta", "alice"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) == printed {
		t.Error("the same user id in another tenant hashed the same")
	}
}

func TestOtelUserHash_RefusesWithoutASecretKey(t *testing.T) {
	t.Setenv("LOOMCYCLE_SECRET_KEY", "")
	var out, errOut bytes.Buffer
	if code := runOtel([]string{"user-hash", "acme", "alice"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "LOOMCYCLE_SECRET_KEY") {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	if code := runOtel([]string{"user-hash", "acme"}, &out, &errOut); code != 2 {
		t.Fatalf("a missing argument exited %d, want 2", code)
	}
}
