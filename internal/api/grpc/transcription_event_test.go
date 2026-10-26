package grpc

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A transcription frame carries what the pass spent, so a gRPC client sees the
// same record the SSE stream and the transcript do. Cost keeps its presence: an
// unpriced pair (nil) is not a free one (0).
func TestEventToProto_TranscriptionCarriesTheRecord(t *testing.T) {
	cost := 0.04
	out := eventToProto(providers.Event{Type: providers.EventTranscription, Transcription: &providers.TranscriptionInfo{
		Ref: "volume:inbox:documents/scan.pdf", Out: "volume:inbox:documents/scan.ocr.md",
		Purpose: "ocr", Engine: "vision", Provider: "anthropic", Model: "claude-haiku-5-5",
		Pages: 12, Seconds: 1.5, InputTokens: 9000, OutputTokens: 4000,
		Cost: &cost, Currency: "USD", Status: "partial", DryRun: true, RunID: "run-2", Error: "max_pages_per_run reached",
	}})
	tr := out.GetTranscription()
	if out.GetType() != "transcription" || tr == nil {
		t.Fatalf("event = %+v", out)
	}
	if tr.GetRef() != "volume:inbox:documents/scan.pdf" || tr.GetOut() != "volume:inbox:documents/scan.ocr.md" ||
		tr.GetPurpose() != "ocr" || tr.GetEngine() != "vision" || tr.GetProvider() != "anthropic" ||
		tr.GetModel() != "claude-haiku-5-5" || tr.GetPages() != 12 || tr.GetSeconds() != 1.5 ||
		tr.GetInputTokens() != 9000 || tr.GetOutputTokens() != 4000 || tr.Cost == nil || tr.GetCost() != 0.04 ||
		tr.GetCurrency() != "USD" || tr.GetStatus() != "partial" || !tr.GetDryRun() || tr.GetRunId() != "run-2" || tr.GetError() != "max_pages_per_run reached" {
		t.Errorf("transcription = %+v", tr)
	}

	unpriced := eventToProto(providers.Event{Type: providers.EventTranscription, Transcription: &providers.TranscriptionInfo{
		Ref: "volume:inbox:a.mp3", Purpose: "stt", Engine: "command", Seconds: 2550, Status: "ok",
	}}).GetTranscription()
	if unpriced == nil || unpriced.Cost != nil {
		t.Errorf("an unpriced pass reads as priced: %+v", unpriced)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "hi"}).GetTranscription() != nil {
		t.Error("a text frame carries a transcription")
	}
}

// Every field of the Go record reaches the frame. Derived from both sides: a
// field added to providers.TranscriptionInfo and to the proto, but not to
// eventToProto, would otherwise compile and arrive empty.
func TestEventToProto_TranscriptionMapsEveryField(t *testing.T) {
	var in providers.TranscriptionInfo
	rv := reflect.ValueOf(&in).Elem()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int:
			f.SetInt(7)
		case reflect.Float64:
			f.SetFloat(1.5)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		default:
			t.Fatalf("field %s has kind %s this test cannot fill — extend it", rv.Type().Field(i).Name, f.Kind())
		}
	}
	msg := eventToProto(providers.Event{Type: providers.EventTranscription, Transcription: &in}).GetTranscription().ProtoReflect()
	fields := msg.Descriptor().Fields()
	if fields.Len() != rv.NumField() {
		t.Errorf("proto TranscriptionInfo has %d fields, providers.TranscriptionInfo %d", fields.Len(), rv.NumField())
	}
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); !msg.Has(fd) {
			t.Errorf("eventToProto leaves %s unset", fd.Name())
		}
	}
}
