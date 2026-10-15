package config

import (
	"fmt"
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
	return nil
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
