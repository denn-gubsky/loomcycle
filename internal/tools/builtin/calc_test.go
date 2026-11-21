package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/calc"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func execCalc(t *testing.T, c *Calc, in string) (tools.Result, map[string]any) {
	t.Helper()
	res, err := c.Execute(context.Background(), json.RawMessage(in))
	if err != nil {
		t.Fatalf("Execute(%s): %v", in, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("Execute(%s): text is not JSON: %v\n%s", in, err, res.Text)
	}
	return res, out
}

func TestCalc_ReturnsTheResultTextAndAssignedNames(t *testing.T) {
	res, out := execCalc(t, &Calc{}, `{"expr":"subtotal = 1249.99 * 3; tax = round(subtotal * 0.2, 2); subtotal + tax"}`)
	if res.IsError {
		t.Fatalf("refused: %s", res.Text)
	}
	if out["text"] != "4499.96" || out["type"] != "number" || out["exact"] != true {
		t.Errorf("result = %s", res.Text)
	}
	// The JSON number keeps its digits: the result is never routed through a
	// float64 on the way out.
	if !strings.Contains(res.Text, `"result":4499.96`) {
		t.Errorf("result field = %s", res.Text)
	}
	assigned, _ := out["assigned"].(map[string]any)
	if assigned["subtotal"] != 3749.97 || assigned["tax"] != 749.99 {
		t.Errorf("assigned = %v", out["assigned"])
	}
	_, out = execCalc(t, &Calc{}, `{"expr":"1/3","digits":5}`)
	if out["text"] != "0.33333" || out["fraction"] != "1/3" {
		t.Errorf("1/3 at 5 digits = %v", out)
	}
	_, out = execCalc(t, &Calc{}, `{"expr":"[randint(1, 100), randint(1, 100)]","seed":1}`)
	_, again := execCalc(t, &Calc{}, `{"expr":"[randint(1, 100), randint(1, 100)]","seed":1}`)
	if out["text"] != again["text"] {
		t.Errorf("same seed, different draws: %v then %v", out["text"], again["text"])
	}
}

// A refusal is JSON the model reads field by field, classified, with the
// evaluator's hint as the next step.
func TestCalc_RefusalIsClassifiedJSONWithTheHint(t *testing.T) {
	res, out := execCalc(t, &Calc{}, `{"expr":"2**10"}`)
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryValidation {
		t.Fatalf("2**10: %+v", res)
	}
	if out["error"] != "syntax" || out["detail"] == "" || !strings.Contains(out["hint"].(string), "^") || out["pos"] != float64(1) {
		t.Errorf("2**10 refusal = %s", res.Text)
	}
	if res.Error.Description != out["hint"] {
		t.Errorf("next step = %q, want the hint %q", res.Error.Description, out["hint"])
	}
	res, out = execCalc(t, &Calc{}, `{"expr":"1/0"}`)
	if !res.IsError || res.Error.Category != tools.CategoryBusiness || out["error"] != "division_by_zero" {
		t.Errorf("1/0: %s %+v", res.Text, res.Error)
	}
	if !strings.Contains(res.Error.Description, "Calc/errors") {
		t.Errorf("a refusal without a hint points at nothing: %q", res.Error.Description)
	}
	res, out = execCalc(t, &Calc{}, `{"vars":{"x":1}}`)
	if out["error"] != "bad_request" || res.Error.Category != tools.CategoryValidation {
		t.Errorf("missing expr: %s", res.Text)
	}
	res, out = execCalc(t, &Calc{}, `{"expr":"1","format":"x"}`)
	if out["error"] != "bad_request" || !strings.Contains(out["detail"].(string), "format") {
		t.Errorf("unknown argument straight to Execute: %s", res.Text)
	}
}

// Every code the evaluator can return is decided here; a new code fails until
// it is given a category.
func TestCalcCategory_MapsEveryErrorCode(t *testing.T) {
	codes := map[string]bool{}
	for _, code := range calc.ErrorCodes() {
		codes[code] = true
		if _, ok := calcCategory[code]; !ok {
			t.Errorf("calc error code %q has no category in calcCategory", code)
		}
	}
	for code := range calcCategory {
		if !codes[code] {
			t.Errorf("calcCategory decides %q, which calc.ErrorCodes() does not return", code)
		}
	}
}

func TestCalc_RefusalsAreCountedByCode(t *testing.T) {
	c := &Calc{}
	for _, in := range []string{`{"expr":"1/0"}`, `{"expr":"2x"}`, `{"expr":"1/0"}`, `{"expr":"2+2"}`} {
		execCalc(t, c, in)
	}
	got := c.RefusalStats()
	if got["division_by_zero"] != 2 || got["syntax"] != 1 || len(got) != 2 {
		t.Errorf("stats = %v, want division_by_zero 2 and syntax 1 only", got)
	}
}

// The run ending is not a refusal of the expression: the loop gets ctx's error.
func TestCalc_CanceledRunReturnsTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Calc{}).Execute(ctx, json.RawMessage(`{"expr":"2+2"}`))
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
