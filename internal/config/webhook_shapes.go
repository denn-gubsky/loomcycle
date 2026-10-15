package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// An event's shape is a list of rules its signed body must meet, each one
// string in one of three forms:
//
//	$.comment.id              the body holds the path (not null)
//	!$.comment                the body does not: missing, or null
//	$.object_kind == "push"   the body holds the path, and its value equals a
//	                          JSON string, number or boolean, compared typed
//
// A list of paths alone can tell an event only from events whose bodies lack
// one of them: an issue_comment body holds every path of an issues body. The
// other two forms say what tells such events apart. The list stays closed —
// there is no other operator and no combining — so what a shape lets through
// can be read off it.

type shapeRule struct {
	segs   []jsonpath.Seg
	absent bool
	equals bool
	value  any
}

const shapeEquals = "=="

// parseShapeRule reads one rule of a shape.
func parseShapeRule(s string) (shapeRule, error) {
	s = strings.TrimSpace(s)
	var r shapeRule
	path := s
	switch {
	case strings.HasPrefix(s, "!"):
		r.absent = true
		path = s[1:]
	case strings.Contains(s, shapeEquals):
		i := strings.Index(s, shapeEquals)
		path = s[:i]
		lit := strings.TrimSpace(s[i+len(shapeEquals):])
		var v any
		if err := json.Unmarshal([]byte(lit), &v); err != nil {
			return r, fmt.Errorf("%q: the value after == is not a JSON string, number or boolean (a string is quoted)", s)
		}
		sv, err := filterScalar(v)
		if err != nil {
			return r, fmt.Errorf("%q: %v", s, err)
		}
		r.equals, r.value = true, sv
	}
	segs, err := jsonpath.Parse(path)
	if err != nil {
		return r, fmt.Errorf("%q is not a supported JSONPath ($, .key and [N] only): %v", s, err)
	}
	if len(segs) == 0 {
		return r, fmt.Errorf("%q names the whole body, which every delivery has", s)
	}
	r.segs = segs
	return r, nil
}

func (r shapeRule) holds(doc any) bool {
	got, ok := jsonpath.Eval(doc, r.segs)
	present := ok && got != nil
	switch {
	case r.absent:
		return !present
	case r.equals:
		return present && filterSameType(got, r.value) && got == r.value
	}
	return present
}

// checkShape checks an event's shape when a definition is written: every
// rule parses, and at least one says what the body holds — a shape of
// absences alone fits almost any body.
func checkShape(rules []string) error {
	positive := false
	for _, s := range rules {
		r, err := parseShapeRule(s)
		if err != nil {
			return err
		}
		positive = positive || !r.absent
	}
	if !positive {
		return fmt.Errorf("needs at least one rule that the body holds a path; absences alone fit almost any body")
	}
	return nil
}

// ShapeHolds reports whether doc, a decoded signed body, meets every rule of
// shape. A rule that does not parse (one that reached the store without the
// check when written) does not hold, nor does an empty shape: an event never
// counts on its unsigned header alone.
func ShapeHolds(shape []string, doc any) bool {
	if len(shape) == 0 {
		return false
	}
	for _, s := range shape {
		r, err := parseShapeRule(s)
		if err != nil || !r.holds(doc) {
			return false
		}
	}
	return true
}
