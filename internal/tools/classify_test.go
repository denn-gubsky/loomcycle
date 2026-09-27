package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

var errQuota = errors.New("memory quota exceeded")

// goErrStub returns a Go error, as a tool does for a failure it did not shape.
type goErrStub struct{ pointerStub }

func (g *goErrStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{}, errQuota
}

// A Go error a tool returns reaches the model classified when the dispatcher
// has a classifier — the same category the transports give that error. Without
// one it stays unclassified, as it always was.
func TestExecute_AToolsGoErrorIsClassified(t *testing.T) {
	tl := &goErrStub{pointerStub{name: "Memory", schema: `{"type":"object"}`}}

	d := NewDispatcher([]Tool{tl})
	d.SetErrorClassifier(func(err error) (ErrorInfo, bool) {
		if errors.Is(err, errQuota) {
			return ErrorInfo{Category: CategoryBusiness, Description: "Free space or raise the quota."}, true
		}
		return ErrorInfo{}, false
	})
	res, goErr := d.Call(context.Background(), "Memory", json.RawMessage(`{"op":"set"}`))
	if !errors.Is(goErr, errQuota) {
		t.Errorf("Call dropped the typed error the transports map to a status: %v", goErr)
	}
	if !res.IsError || res.Error == nil || res.Error.Category != CategoryBusiness || res.Error.Description == "" {
		t.Errorf("result = %+v (error %+v); want the business classification", res, res.Error)
	}

	plain := NewDispatcher([]Tool{tl}).Execute(context.Background(), "Memory", json.RawMessage(`{"op":"set"}`))
	if plain.Error != nil {
		t.Errorf("classified without a classifier: %+v", plain.Error)
	}
}

// A call to a tool the run does not have is a validation failure: the name is
// the input, and the caller fixes it.
func TestExecute_AnUnknownToolIsAValidationFailure(t *testing.T) {
	res := NewDispatcher(nil).Execute(context.Background(), "Nope", json.RawMessage(`{}`))
	if !res.IsError || res.Error == nil || res.Error.Category != CategoryValidation || res.Error.Retryable {
		t.Errorf("unknown tool: %+v (error %+v)", res, res.Error)
	}
}
