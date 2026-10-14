package config

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// A webhook's filters are conditions on the signed body that every delivery
// must meet to start anything: a push to a tag, or a branch deletion, is
// answered 202 filtered by the receiver instead of starting a run that would
// only decide to do nothing. The list of operators is closed — eq, ne, glob,
// in — with no expression language and no regular expressions, so what a
// filter lets through can be read off the definition.
//
// A value is a literal, or `value_from: metadata.<key>`, read from the
// definition's own metadata. Nothing is ever taken from the body but the
// value under test.
//
// Comparison is typed: a string never equals a number. A path that is
// missing, null, or of another type than the value is a miss for every
// operator, ne included: a filter that cannot be evaluated must not start a
// run.

// WebhookFilter is one condition a delivery's signed body must meet.
type WebhookFilter struct {
	// Path is a JSONPath into the body ($, .key and [N] only).
	Path string `json:"path,omitempty" yaml:"path"`
	// Op is eq, ne, glob or in.
	Op string `json:"op,omitempty" yaml:"op"`
	// Value is a string, number or boolean (a list of them for in; a string
	// pattern for glob). Exactly one of Value and ValueFrom is set.
	Value any `json:"value,omitempty" yaml:"value"`
	// ValueFrom is "metadata.<key>": the value is the definition's metadata
	// entry of that key.
	ValueFrom string `json:"value_from,omitempty" yaml:"value_from"`
}

const (
	maxWebhookFilters      = 32
	maxWebhookFilterIn     = 64
	maxWebhookGlobPattern  = 256
	maxWebhookFilterString = 1024
	webhookValueFromPrefix = "metadata."
)

// CheckWebhookFilters checks a definition's filters when it is written (the
// yaml load and the WebhookDef tool both call it): each path parses, each
// operator is one of the four, and each value — a literal, or the metadata
// entry value_from names — has a type its operator takes.
func CheckWebhookFilters(w Webhook) error {
	if len(w.Filters) > maxWebhookFilters {
		return fmt.Errorf("filters holds %d filters; at most %d", len(w.Filters), maxWebhookFilters)
	}
	for i, f := range w.Filters {
		if err := checkWebhookFilter(w, f); err != nil {
			return fmt.Errorf("filters[%d]: %w", i, err)
		}
	}
	return nil
}

func checkWebhookFilter(w Webhook, f WebhookFilter) error {
	segs, err := jsonpath.Parse(f.Path)
	if err != nil {
		return fmt.Errorf("path %q is not a supported JSONPath ($, .key and [N] only): %v", f.Path, err)
	}
	if len(segs) == 0 {
		return fmt.Errorf("path %q names the whole body, which is never a string, number or boolean", f.Path)
	}
	switch f.Op {
	case "eq", "ne", "glob", "in":
	default:
		return fmt.Errorf("op %q is not one of eq, ne, glob, in", f.Op)
	}
	v := f.Value
	switch {
	case f.Value != nil && f.ValueFrom != "":
		return fmt.Errorf("set value or value_from, not both")
	case f.ValueFrom != "":
		key, ok := strings.CutPrefix(f.ValueFrom, webhookValueFromPrefix)
		if !ok || key == "" {
			return fmt.Errorf("value_from %q is not metadata.<key>", f.ValueFrom)
		}
		mv, ok := w.Metadata[key]
		if !ok {
			return fmt.Errorf("value_from %q: the definition's metadata has no key %q", f.ValueFrom, key)
		}
		v = mv
	case f.Value == nil:
		return fmt.Errorf("needs a value (a string, number or boolean) or value_from: metadata.<key>; null is not a value")
	}
	if _, err := filterOperand(f.Op, v); err != nil {
		if f.ValueFrom != "" {
			return fmt.Errorf("value_from %q: %w", f.ValueFrom, err)
		}
		return err
	}
	return nil
}

// filterOperand returns v in the form op compares with: a scalar (string,
// float64 or bool) for eq and ne, a string for glob, a list of scalars for
// in. Numbers of any Go type — a yaml load gives int, JSON float64 — become
// float64, the type a decoded body's numbers have.
func filterOperand(op string, v any) (any, error) {
	switch op {
	case "glob":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("glob takes a string pattern, not %s", filterTypeName(v))
		}
		if len(s) > maxWebhookGlobPattern {
			return nil, fmt.Errorf("a glob pattern is at most %d bytes", maxWebhookGlobPattern)
		}
		return s, nil
	case "in":
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("in takes a list of strings, numbers or booleans, not %s", filterTypeName(v))
		}
		if len(list) == 0 || len(list) > maxWebhookFilterIn {
			return nil, fmt.Errorf("in takes 1 to %d values", maxWebhookFilterIn)
		}
		out := make([]any, len(list))
		for i, e := range list {
			s, err := filterScalar(e)
			if err != nil {
				return nil, fmt.Errorf("in[%d]: %w", i, err)
			}
			out[i] = s
		}
		return out, nil
	default:
		return filterScalar(v)
	}
}

func filterScalar(v any) (any, error) {
	switch t := v.(type) {
	case string:
		if len(t) > maxWebhookFilterString {
			return nil, fmt.Errorf("a string value is at most %d bytes", maxWebhookFilterString)
		}
		return t, nil
	case bool:
		return t, nil
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, fmt.Errorf("a number must be finite")
		}
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int8:
		return float64(t), nil
	case int16:
		return float64(t), nil
	case int32:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case uint:
		return float64(t), nil
	case uint8:
		return float64(t), nil
	case uint16:
		return float64(t), nil
	case uint32:
		return float64(t), nil
	case uint64:
		return float64(t), nil
	}
	return nil, fmt.Errorf("a value is a string, number or boolean, not %s", filterTypeName(v))
}

func filterTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// FirstFailingFilter returns the index of the first of w's filters that doc
// — the decoded signed body — does not meet, or -1 when every one holds (or
// there are none).
func (w Webhook) FirstFailingFilter(doc any) int {
	for i, f := range w.Filters {
		if !w.filterHolds(f, doc) {
			return i
		}
	}
	return -1
}

func (w Webhook) filterHolds(f WebhookFilter, doc any) bool {
	segs, err := jsonpath.Parse(f.Path)
	if err != nil || len(segs) == 0 {
		return false
	}
	got, ok := jsonpath.Eval(doc, segs)
	if !ok || got == nil {
		return false
	}
	v := f.Value
	if f.ValueFrom != "" {
		// Checked when written; a definition that reached the store some
		// other way, or whose metadata lost the key, misses.
		key, _ := strings.CutPrefix(f.ValueFrom, webhookValueFromPrefix)
		if v, ok = w.Metadata[key]; !ok {
			return false
		}
	}
	want, err := filterOperand(f.Op, v)
	if err != nil {
		return false
	}
	switch f.Op {
	case "eq":
		return filterSameType(got, want) && got == want
	case "ne":
		return filterSameType(got, want) && got != want
	case "glob":
		s, ok := got.(string)
		return ok && GlobMatch(want.(string), s)
	case "in":
		for _, e := range want.([]any) {
			if filterSameType(got, e) && got == e {
				return true
			}
		}
	}
	return false
}

// filterSameType reports whether a body value and a filter's scalar have
// the same JSON type. A body value that is an object or a list never does.
func filterSameType(got, want any) bool {
	switch want.(type) {
	case string:
		_, ok := got.(string)
		return ok
	case float64:
		_, ok := got.(float64)
		return ok
	case bool:
		_, ok := got.(bool)
		return ok
	}
	return false
}

// GlobMatch reports whether s matches pattern, in which * matches any run of
// characters (none included, / included) and ? exactly one; every other
// character, [ and \ among them, matches itself.
//
// The pattern is split at each *: the first piece must start s, the last
// must end it, and each piece between is matched at its leftmost place after
// the one before — which, with no other wildcard, finds a match whenever
// there is one. A piece without ? is found with strings.Index, so the common
// patterns cost a scan of s; a piece with ? costs at most len(s) times its
// length.
func GlobMatch(pattern, s string) bool {
	pieces := strings.Split(pattern, "*")
	if len(pieces) == 1 {
		end, ok := globPieceAt(s, 0, pattern)
		return ok && end == len(s)
	}
	pos, ok := globPieceAt(s, 0, pieces[0])
	if !ok {
		return false
	}
	last := pieces[len(pieces)-1]
	start, ok := globRunesBack(s, utf8.RuneCountInString(last))
	if !ok || start < pos {
		return false
	}
	if end, ok := globPieceAt(s, start, last); !ok || end != len(s) {
		return false
	}
	within := s[:start] // the middle pieces fit before the last one
	for _, p := range pieces[1 : len(pieces)-1] {
		if p == "" {
			continue
		}
		end, ok := globFind(within, pos, p)
		if !ok {
			return false
		}
		pos = end
	}
	return true
}

// globPieceAt matches piece (literal characters and ?) at s[i:], returning
// where the match ends.
func globPieceAt(s string, i int, piece string) (int, bool) {
	for _, pr := range piece {
		if i >= len(s) {
			return 0, false
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if pr != '?' && pr != r {
			return 0, false
		}
		i += size
	}
	return i, true
}

// globFind finds piece's leftmost match in s at or after from, returning
// where it ends.
func globFind(s string, from int, piece string) (int, bool) {
	if !strings.Contains(piece, "?") {
		j := strings.Index(s[from:], piece)
		if j < 0 {
			return 0, false
		}
		return from + j + len(piece), true
	}
	for i := from; i < len(s); {
		if end, ok := globPieceAt(s, i, piece); ok {
			return end, true
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return 0, false
}

// globRunesBack returns the index n runes before the end of s.
func globRunesBack(s string, n int) (int, bool) {
	i := len(s)
	for ; n > 0; n-- {
		if i == 0 {
			return 0, false
		}
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return i, true
}

// CheckWebhookMappingPaths reports the first payload_mapping path that does
// not parse, in target order. Such a path answers every delivery 400
// bad_mapping, so the WebhookDef tool refuses it at create and fork. Not part
// of the checks a snapshot restore or the yaml load re-runs, which must accept
// what they accepted before: a static definition gets a boot warning instead.
func CheckWebhookMappingPaths(mapping map[string]string) error {
	for _, target := range sortedVarNames(mapping) {
		if _, err := jsonpath.Parse(mapping[target]); err != nil {
			return fmt.Errorf("payload_mapping[%q] = %q is not a supported JSONPath ($, .key and [N] only): %v", target, mapping[target], err)
		}
	}
	return nil
}
