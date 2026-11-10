package otel

import (
	"path"
	"sync/atomic"
)

// ModelLabelOther is the model label of a call whose model the operator did
// not configure.
const ModelLabelOther = "other"

// modelLabelSource is where the model labels come from: current returns the
// operator's configuration identity (it changes when the config is
// reloaded), build the configured model names and globs for one identity.
type modelLabelSource struct {
	current func() any
	build   func(any) (names, patterns []string)
	cache   atomic.Pointer[modelLabelSet]
}

type modelLabelSet struct {
	version  any
	names    map[string]bool
	patterns []string
}

// labelSource is set once by the binary, beside the meter provider. Nil (no
// source installed, as in most tests) leaves model names as they are.
var labelSource atomic.Pointer[modelLabelSource]

// SetModelLabelSource bounds the model label of every instrument to the
// models the operator configured: a model build does not name is counted as
// "other". A model name reaches the metrics from callers too — a gateway
// request's pinned model, a tenant-authored agent's — and a series, once
// made, lasts until restart: without this bound any caller could spend the
// cardinality limit with invented names and push the operator's own models
// into the overflow series. current is read on every recording and build
// runs again whenever it returns a new value. Returns the restore function.
func SetModelLabelSource(current func() any, build func(any) (names, patterns []string)) func() {
	prev := labelSource.Swap(&modelLabelSource{current: current, build: build})
	return func() { labelSource.Store(prev) }
}

// metricModel is model's label: itself when the operator configured that
// exact name; the pattern string when only a model_pattern glob admits it;
// else "other". A model a glob admits is labelled by the glob, never by its
// own name: "claude-*" matches any string a caller can invent, so the name
// would bring back the unbounded label this function exists to stop. The
// pattern string is the operator's, so it is bounded. Patterns are matched
// whatever the provider: the label is bounded either way.
func metricModel(model string) string {
	src := labelSource.Load()
	if src == nil || model == "" {
		return model
	}
	v := src.current()
	set := src.cache.Load()
	if set == nil || set.version != v {
		names, patterns := src.build(v)
		set = &modelLabelSet{version: v, names: make(map[string]bool, len(names)), patterns: patterns}
		for _, n := range names {
			set.names[n] = true
		}
		src.cache.Store(set)
	}
	if set.names[model] {
		return model
	}
	for _, p := range set.patterns {
		if ok, _ := path.Match(p, model); ok {
			return p
		}
	}
	return ModelLabelOther
}
