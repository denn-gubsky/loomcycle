package calc

import (
	"context"
	"encoding/json"
	"testing"
)

func evalText(t *testing.T, expr string) (string, *Error) {
	t.Helper()
	r, err := Eval(context.Background(), Request{Expr: expr})
	return r.Text, err
}

func mustEval(t *testing.T, expr string) Response {
	t.Helper()
	r, err := Eval(context.Background(), Request{Expr: expr})
	if err != nil {
		t.Fatalf("Eval(%q): %v", expr, err)
	}
	return r
}

func mustFail(t *testing.T, expr string, code errCode) *Error {
	t.Helper()
	r, err := Eval(context.Background(), Request{Expr: expr})
	if err == nil {
		t.Fatalf("Eval(%q) = %q, want error %s", expr, r.Text, code)
	}
	if err.Code != string(code) {
		t.Fatalf("Eval(%q) code = %s (%v), want %s", expr, err.Code, err, code)
	}
	return err
}

func rawVars(t *testing.T, m map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

type textCase struct {
	expr string
	want string
}

func checkTexts(t *testing.T, cases []textCase) {
	t.Helper()
	for _, c := range cases {
		got, err := evalText(t, c.expr)
		if err != nil {
			t.Errorf("Eval(%q): %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("Eval(%q) = %q, want %q", c.expr, got, c.want)
		}
	}
}
