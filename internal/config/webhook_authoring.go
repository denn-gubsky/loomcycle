package config

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// CheckWebhookAuthoring checks what a webhook definition being written must
// meet beyond the checks a stored one is re-run through. A snapshot restore
// re-runs CheckWebhookEvents, CheckWebhookFilters and CheckWebhookDedupKey and
// must accept what they accepted, so these refusals live apart: the yaml load
// and the WebhookDef tool's create and fork call this, nothing else.
//
// A path in an event's shape, a filter or the dedup_key never names a key
// holding whitespace, `=` or a quote. The path grammar takes any of them in a
// key, so a mistyped rule reads as a key no body has: `!$.x == 1` is "no key
// named `x == 1`", which every body meets, and the event check it was meant to
// be passes everything.
//
// A number a filter or a shape's == compares with is at most 2^53-1 in
// magnitude (maxExactWebhookNumber).
func CheckWebhookAuthoring(w Webhook) error {
	events := make([]string, 0, len(w.EventShapes))
	for e := range w.EventShapes {
		events = append(events, e)
	}
	sort.Strings(events)
	for _, e := range events {
		for _, rule := range w.EventShapes[e] {
			if err := checkAuthoredShapeRule(rule); err != nil {
				return fmt.Errorf("event_shapes[%q]: %w", e, err)
			}
		}
	}
	for i, f := range w.Filters {
		if err := checkAuthoredPathKeys(f.Path); err != nil {
			return fmt.Errorf("filters[%d]: %w", i, err)
		}
		if err := checkAuthoredFilterNumbers(w, f); err != nil {
			return fmt.Errorf("filters[%d]: %w", i, err)
		}
	}
	for i, p := range w.DedupKey {
		if err := checkAuthoredPathKeys(p); err != nil {
			return fmt.Errorf("dedup_key[%d]: %w", i, err)
		}
	}
	return nil
}

// checkAuthoredShapeRule refuses a rule that is an absence and an equality at
// once, which parseShapeRule reads as an absence of a key spelt with the
// comparison in it, and a rule whose path names such a key.
func checkAuthoredShapeRule(rule string) error {
	s := strings.TrimSpace(rule)
	if strings.HasPrefix(s, "!") && strings.Contains(s, shapeEquals) {
		return fmt.Errorf("%q: a rule is a path, !path or path == value; ! and == do not combine", rule)
	}
	path := strings.TrimPrefix(s, "!")
	if i := strings.Index(path, shapeEquals); i >= 0 {
		path = path[:i]
	}
	if err := checkAuthoredPathKeys(path); err != nil {
		return fmt.Errorf("%q: %w", rule, err)
	}
	if r, err := parseShapeRule(s); err == nil && r.equals {
		if err := checkExactNumber(r.value); err != nil {
			return fmt.Errorf("%q: %w", rule, err)
		}
	}
	return nil
}

// maxExactWebhookNumber is the largest magnitude a number compared with a
// body's is written as. A body's numbers are decoded to float64; at 2^53 and
// above, distinct integers decode alike, so an id 2^53+1 would equal a literal
// 2^53. Below it, an integer in a body equals the literal only when it is
// that integer.
const maxExactWebhookNumber = 1<<53 - 1

// checkExactNumber refuses a number literal (filterScalar's float64) whose
// magnitude is past maxExactWebhookNumber. Other values pass.
func checkExactNumber(v any) error {
	if f, ok := v.(float64); ok && math.Abs(f) > maxExactWebhookNumber {
		return fmt.Errorf("the number %v is past %d in magnitude, beyond which two different numbers in a body compare equal", f, int64(maxExactWebhookNumber))
	}
	return nil
}

// checkAuthoredFilterNumbers applies checkExactNumber to a filter's value —
// a literal, or the metadata entry value_from names — and each value of in.
// A value of another type is left to CheckWebhookFilters.
func checkAuthoredFilterNumbers(w Webhook, f WebhookFilter) error {
	v := f.Value
	if f.ValueFrom != "" {
		key, _ := strings.CutPrefix(f.ValueFrom, webhookValueFromPrefix)
		v = w.Metadata[key]
	}
	operand, err := filterOperand(f.Op, v)
	if err != nil {
		return nil
	}
	if list, ok := operand.([]any); ok {
		for i, e := range list {
			if err := checkExactNumber(e); err != nil {
				return fmt.Errorf("in[%d]: %w", i, err)
			}
		}
		return nil
	}
	return checkExactNumber(operand)
}

// checkAuthoredPathKeys refuses a path with a key holding whitespace, `=` or
// a quote. A path that does not parse is left to the check that reports it.
func checkAuthoredPathKeys(path string) error {
	segs, err := jsonpath.Parse(path)
	if err != nil {
		return nil
	}
	for _, sg := range segs {
		if sg.IsIndex {
			continue
		}
		if strings.IndexFunc(sg.Key, func(r rune) bool {
			return unicode.IsSpace(r) || r == '=' || r == '"' || r == '\''
		}) >= 0 {
			return fmt.Errorf("path %q has a key %q holding whitespace, = or a quote: far likelier a mistyped rule than a field, and read as a key no body has", path, sg.Key)
		}
	}
	return nil
}
