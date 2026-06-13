package store

import (
	"encoding/json"
	"testing"
)

// TestBackfillSystemPromptBase pins the contract of the transform hoisted from
// the sqlite + postgres adapters: it copies system_prompt → system_prompt_base
// only when the base is empty and a prompt exists, and is otherwise a no-op.
func TestBackfillSystemPromptBase(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOK   bool
		wantErr  bool
		wantBase string // expected system_prompt_base in the output (when ok)
	}{
		{
			name:     "fills base from prompt when base empty",
			in:       `{"model":"x","system_prompt":"be helpful"}`,
			wantOK:   true,
			wantBase: "be helpful",
		},
		{
			name:   "no-op when base already set",
			in:     `{"system_prompt":"p","system_prompt_base":"already"}`,
			wantOK: false,
		},
		{
			name:   "no-op when neither prompt nor base present",
			in:     `{"model":"x"}`,
			wantOK: false,
		},
		{
			name:    "error on malformed JSON",
			in:      `{"system_prompt":`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok, err := BackfillSystemPromptBase([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (out=%s ok=%v)", out, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if out != nil {
					t.Errorf("no-op case returned non-nil out: %s", out)
				}
				return
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}
			if got, _ := raw["system_prompt_base"].(string); got != tc.wantBase {
				t.Errorf("system_prompt_base = %q, want %q", got, tc.wantBase)
			}
		})
	}
}
