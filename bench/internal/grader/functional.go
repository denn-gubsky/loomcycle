package grader

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
	"github.com/denn-gubsky/loomcycle/bench/internal/runner"
)

// Functional grades the tool-call trace against the case's expected
// sequence. Each expected entry can constrain:
//
//   - tool name (exact)
//   - argument shape: args_must_include map with these keys:
//     "<field>": <value>        — exact field == value
//     "<field>_contains": substr — field contains substring
//     "has_<field>": true       — field present, any value
//     "<field>_is_object": true — field is a non-empty object
//   - min/max call count for the named tool
//
// Plus case-level OrderStrict (calls must appear in declared order)
// and ForbidRepeatCalls (no two adjacent calls with identical args).
func Functional(events []runner.ProviderEvent, exp cases.Functional) AxisResult {
	calls := extractToolCalls(events)

	r := AxisResult{Pass: true, Score: 1.0}

	if len(exp.ToolCalls) == 0 && len(calls) == 0 {
		return r // case expected nothing, model did nothing — pass
	}

	// Bucket calls by tool name for count + arg checks.
	callsByName := make(map[string][]toolCall, len(calls))
	for _, c := range calls {
		callsByName[c.Name] = append(callsByName[c.Name], c)
	}

	// Per-expectation checks.
	for _, want := range exp.ToolCalls {
		got := callsByName[want.Name]
		min := want.MinCalls
		max := want.MaxCalls
		if min == 0 && max == 0 {
			min, max = 1, 1
			if want.ArgsMustInclude == nil {
				// No args constraints + no count constraints = at
				// least one call required.
				min = 1
				max = 0 // 0 = no upper bound
			}
		}
		if len(got) < min {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s: %d calls < min %d", want.Name, len(got), min))
			continue
		}
		if max > 0 && len(got) > max {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s: %d calls > max %d", want.Name, len(got), max))
			continue
		}
		// Args check — pass when AT LEAST ONE call's args satisfy
		// the constraints (handles the "model also made an extra
		// throwaway call with weird args" case without false fails).
		if len(want.ArgsMustInclude) > 0 {
			matched := false
			for _, c := range got {
				if matchArgs(c.Args, want.ArgsMustInclude) {
					matched = true
					break
				}
			}
			if !matched {
				r.Pass = false
				r.Score = 0
				r.Reasons = append(r.Reasons,
					fmt.Sprintf("%s: no call matched the args constraints %v", want.Name, want.ArgsMustInclude))
			}
		}
	}

	// Order check.
	if exp.OrderStrict {
		if !checkOrder(calls, exp.ToolCalls) {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, "tool calls not in expected order")
		}
	}

	// Forbid-repeats check.
	if exp.ForbidRepeatCalls {
		for i := 1; i < len(calls); i++ {
			if calls[i].Name == calls[i-1].Name && string(calls[i].RawArgs) == string(calls[i-1].RawArgs) {
				r.Pass = false
				r.Score = 0
				r.Reasons = append(r.Reasons,
					fmt.Sprintf("repeated identical call to %s (doom-loop)", calls[i].Name))
				break
			}
		}
	}

	return r
}

// toolCall is the parsed shape of one tool_call event used by the
// grader. RawArgs preserves the original JSON for the
// forbid-repeat-calls equality check.
type toolCall struct {
	Name    string
	Args    map[string]any
	RawArgs json.RawMessage
}

func extractToolCalls(events []runner.ProviderEvent) []toolCall {
	var calls []toolCall
	for _, ev := range events {
		if ev.Type != "tool_call" || ev.ToolUse == nil {
			continue
		}
		var args map[string]any
		if len(ev.ToolUse.Input) > 0 {
			_ = json.Unmarshal(ev.ToolUse.Input, &args)
		}
		calls = append(calls, toolCall{
			Name:    ev.ToolUse.Name,
			Args:    args,
			RawArgs: ev.ToolUse.Input,
		})
	}
	return calls
}

// matchArgs evaluates the constraint map against one tool's args.
// All constraints must hold.
func matchArgs(args map[string]any, constraints map[string]any) bool {
	if args == nil {
		return false
	}
	for k, want := range constraints {
		switch {
		case strings.HasSuffix(k, "_contains"):
			field := strings.TrimSuffix(k, "_contains")
			got, _ := args[field].(string)
			wantStr, _ := want.(string)
			if !strings.Contains(got, wantStr) {
				return false
			}
		case strings.HasPrefix(k, "has_"):
			field := strings.TrimPrefix(k, "has_")
			if _, present := args[field]; !present {
				return false
			}
		case strings.HasSuffix(k, "_is_object"):
			field := strings.TrimSuffix(k, "_is_object")
			obj, ok := args[field].(map[string]any)
			if !ok || len(obj) == 0 {
				return false
			}
		default:
			got, present := args[k]
			if !present {
				return false
			}
			// Exact equality via normalisation through json — handles
			// int-vs-float comparisons between yaml-decoded ints and
			// json-decoded floats.
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				return false
			}
		}
	}
	return true
}

// checkOrder verifies the actual calls cover the expected names in
// the declared order. Extra calls between expected ones are fine.
func checkOrder(actual []toolCall, expected []cases.ToolCall) bool {
	i := 0
	for _, c := range actual {
		if i >= len(expected) {
			return true
		}
		if c.Name == expected[i].Name {
			i++
		}
	}
	return i >= len(expected)
}
