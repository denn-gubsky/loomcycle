package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Entry is one hook an agent carries: a reference to a HookDef ("name", the
// version active when the run starts, or "name@3", pinned), or an inline
// webhook. On the wire a reference is a string and an inline webhook an object.
type Entry struct {
	Ref    string
	Inline *Inline
}

// Inline is a webhook written into the definition that attaches it — the shape
// the removed registration API used, kept so what was registered moves into an
// AgentDef unchanged. Code is not an inline kind: it lives in a HookDef, which
// is versioned and reviewable.
type Inline struct {
	Name      string   `json:"name" yaml:"name"`
	URL       string   `json:"url" yaml:"url"`
	FailMode  FailMode `json:"fail_mode,omitempty" yaml:"fail_mode,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
	// Headers are sent with each call; a value may name a credential
	// ($cred:<name>), resolved for the run at call time.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// MarshalJSON writes a reference as a string and an inline webhook as an object.
func (e Entry) MarshalJSON() ([]byte, error) {
	if e.Inline != nil {
		return json.Marshal(e.Inline)
	}
	return json.Marshal(e.Ref)
}

// UnmarshalJSON reads a string (a reference) or an object (an inline webhook).
func (e *Entry) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		*e = Entry{}
		return json.Unmarshal(b, &e.Ref)
	}
	var in Inline
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("a hook is a HookDef name or an inline webhook {name, url, fail_mode, timeout_ms}: %w", err)
	}
	*e = Entry{Inline: &in}
	return nil
}

// MarshalYAML mirrors MarshalJSON.
func (e Entry) MarshalYAML() (any, error) {
	if e.Inline != nil {
		return e.Inline, nil
	}
	return e.Ref, nil
}

// UnmarshalYAML mirrors UnmarshalJSON.
func (e *Entry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*e = Entry{Ref: n.Value}
		return nil
	}
	var in Inline
	if err := n.Decode(&in); err != nil {
		return err
	}
	*e = Entry{Inline: &in}
	return nil
}

// Label names the entry in errors and in the chain.
func (e Entry) Label() string {
	if e.Inline != nil {
		return e.Inline.Name
	}
	return e.Ref
}

// EventHooks maps an event (the dispatcher's phase names) to the hooks attached
// for it, in chain order.
type EventHooks map[Phase][]Entry

// ToolHooks maps a tool name to the hooks of that tool — its pre and post hooks
// in that agent.
type ToolHooks map[string]EventHooks

// ParseRef splits a HookDef reference into its name and version (0 = the
// version active when the run starts).
func ParseRef(ref string) (name string, version int, err error) {
	name = ref
	if i := strings.LastIndexByte(ref, '@'); i >= 0 {
		name = ref[:i]
		v, perr := strconv.Atoi(ref[i+1:])
		if perr != nil || v < 1 {
			return "", 0, fmt.Errorf("hook %q: the version after @ must be a positive number", ref)
		}
		version = v
	}
	if err := ValidateDefName(name); err != nil {
		return "", 0, fmt.Errorf("hook %q: %w", ref, err)
	}
	return name, version, nil
}

// Validate checks the shape of an agent's hooks: known events, tool events only
// under a tool, and well-formed entries. Whether a referenced HookDef exists is
// checked where a store is at hand (when an AgentDef is saved, and when a run
// resolves it).
func (e EventHooks) Validate(tool string) error {
	for _, phase := range sortedPhases(e) {
		switch {
		case IsToolPhase(phase):
		case isRunPhase(phase):
			if tool != "" {
				return fmt.Errorf("tool %s: %s is a run event; attach it under the agent's hooks, not a tool's", tool, phase)
			}
		default:
			return fmt.Errorf("hooks: unknown event %q (one of %s)", phase, phaseList)
		}
		for _, entry := range e[phase] {
			if err := entry.validate(); err != nil {
				return fmt.Errorf("hooks.%s: %w", phase, err)
			}
		}
	}
	return nil
}

func (e Entry) validate() error {
	if e.Inline == nil {
		_, _, err := ParseRef(e.Ref)
		return err
	}
	in := e.Inline
	if err := ValidateDefName(in.Name); err != nil {
		return fmt.Errorf("inline webhook: %w", err)
	}
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		return fmt.Errorf("inline webhook %s: url must be http:// or https://", in.Name)
	}
	if in.FailMode != "" && in.FailMode != FailOpen && in.FailMode != FailClosed {
		return fmt.Errorf("inline webhook %s: fail_mode must be %q or %q", in.Name, FailOpen, FailClosed)
	}
	if in.TimeoutMs < 0 {
		return fmt.Errorf("inline webhook %s: timeout_ms must be ≥ 0", in.Name)
	}
	if err := validateHeaders(in.Headers); err != nil {
		return fmt.Errorf("inline webhook %s: %w", in.Name, err)
	}
	return nil
}

// validateHeaders refuses a header name that is not an HTTP token, and the
// headers the webhook call sets itself.
func validateHeaders(h map[string]string) error {
	for k, v := range h {
		if k == "" {
			return fmt.Errorf("a header name is required")
		}
		for _, r := range k {
			if !(r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return fmt.Errorf("header %q: a name is letters, digits, - and _", k)
			}
		}
		switch strings.ToLower(k) {
		case "host", "content-type", "content-length", "accept", "transfer-encoding", "connection":
			return fmt.Errorf("header %q is set by the call itself", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q: a value cannot contain a line break", k)
		}
	}
	return nil
}

// Validate checks every tool's hooks.
func (t ToolHooks) Validate() error {
	names := make([]string, 0, len(t))
	for name := range t {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("tool hooks: a tool name is required")
		}
		if err := t[name].Validate(name); err != nil {
			return err
		}
	}
	return nil
}

func sortedPhases(e EventHooks) []Phase {
	out := make([]Phase, 0, len(e))
	for p := range e {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Source says where a run's hooks come from, which decides where their HookDef
// references resolve and whether they may widen hosts.
type Source struct {
	// Owner labels the hooks in the chain and in the payload ("agent:<name>").
	Owner string
	// Tenant owns the definition; its references resolve there, then in the
	// shared "" tenant. Never the run's tenant: a static agent run in tenant T
	// must not pick up T's HookDef of the same name in place of the operator's.
	Tenant string
	// OperatorAuthored: the definition was written by an operator, which is a
	// precondition for any of its hooks to widen hosts.
	OperatorAuthored bool
}

// LookupDef returns a HookDef version: the active one when version is 0. tenant
// is where the lookup is made; the store's own not-found is returned as is.
type LookupDef func(ctx context.Context, tenant, name string, version int) (Def, string, error)

// Resolve appends an agent's hooks to set, in chain order: each tool's own hooks
// (tools in name order), then the agent-level hooks. A reference resolves in
// the source's tenant, then in the shared tenant; one that resolves nowhere, or
// that declares a different event than the slot it sits in, fails the whole
// resolution — a gate a definition names must not silently vanish.
func Resolve(ctx context.Context, src Source, agent EventHooks, tools ToolHooks, lookup LookupDef, permits Permits, set *Set) error {
	toolNames := make([]string, 0, len(tools))
	for name := range tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)
	for _, tool := range toolNames {
		if err := resolveEvents(ctx, src, tool, tools[tool], lookup, permits, set); err != nil {
			return err
		}
	}
	return resolveEvents(ctx, src, "", agent, lookup, permits, set)
}

func resolveEvents(ctx context.Context, src Source, tool string, events EventHooks, lookup LookupDef, permits Permits, set *Set) error {
	for _, phase := range orderedPhases(events) {
		for _, entry := range events[phase] {
			h, err := resolveEntry(ctx, src, tool, phase, entry, lookup, permits)
			if err != nil {
				return err
			}
			if _, err := set.Register(h); err != nil {
				return fmt.Errorf("hook %s: %w", entry.Label(), err)
			}
		}
	}
	return nil
}

// orderedPhases is a stable phase order; within a phase the listed order is
// kept, which is the chain order.
func orderedPhases(e EventHooks) []Phase { return sortedPhases(e) }

func resolveEntry(ctx context.Context, src Source, tool string, phase Phase, entry Entry, lookup LookupDef, permits Permits) (*Hook, error) {
	h := &Hook{Tenant: src.Tenant, Owner: src.Owner, Phase: phase}
	if tool != "" {
		h.Tools = []string{tool}
	}
	if in := entry.Inline; in != nil {
		h.Name, h.CallbackURL, h.FailMode, h.TimeoutMs, h.Headers = in.Name, in.URL, in.FailMode, in.TimeoutMs, in.Headers
	} else {
		name, version, err := ParseRef(entry.Ref)
		if err != nil {
			return nil, err
		}
		if lookup == nil {
			return nil, fmt.Errorf("hook %s: HookDefs are not available here", entry.Ref)
		}
		def, defID, defTenant, err := lookupInScope(ctx, lookup, src.Tenant, name, version)
		if err != nil {
			return nil, fmt.Errorf("hook %s: %w", entry.Ref, err)
		}
		if def.Event != phase {
			return nil, fmt.Errorf("hook %s answers %s, but is attached under %s", entry.Ref, def.Event, phase)
		}
		h.Name, h.DefID, h.FailMode, h.TimeoutMs = name, defID, def.FailMode, def.TimeoutMs
		h.Tenant = defTenant
		switch def.Body.Kind {
		case BodyKindCode:
			h.Code = def.Body.Code
		default:
			h.CallbackURL, h.Headers = def.Body.URL, def.Body.Headers
		}
		if def.Match != nil && len(def.Match.Tools) > 0 {
			if tool != "" && !globsMatch(def.Match.Tools, tool) {
				return nil, fmt.Errorf("hook %s is attached to %s, which its match.tools %v excludes", entry.Ref, tool, def.Match.Tools)
			}
			if tool == "" {
				h.Tools = def.Match.Tools
			}
		}
	}
	h.WidenPermitted = phase == PhasePre && src.OperatorAuthored && permits.Has(src.Tenant, h.Name)
	return h, nil
}

// lookupInScope looks a reference up in the definition's tenant, then in the
// shared tenant, and says where it was found.
func lookupInScope(ctx context.Context, lookup LookupDef, tenant, name string, version int) (Def, string, string, error) {
	def, id, err := lookup(ctx, tenant, name, version)
	if err == nil || tenant == "" {
		return def, id, tenant, err
	}
	if def, id, serr := lookup(ctx, "", name, version); serr == nil {
		return def, id, "", nil
	}
	return Def{}, "", "", err
}

// LiftToolEntries rewrites a definition's `tools` array so each
// `{name, hooks}` entry becomes its name, with its hooks moved under
// `tool_hooks` (appended after any already there). Everything that reads a
// definition's tools keeps reading names; the hooks travel beside them. A
// definition with no object entries comes back unchanged.
func LiftToolEntries(def json.RawMessage) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(def, &top); err != nil || top == nil {
		return def, nil // not an object: the caller's own decode reports it
	}
	raw, ok := top["tools"]
	if !ok {
		return def, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return def, nil
	}
	var lifted ToolHooks
	names := make([]string, 0, len(items))
	for i, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) > 0 && item[0] == '"' {
			var name string
			if err := json.Unmarshal(item, &name); err != nil {
				return nil, fmt.Errorf("tools[%d]: %w", i, err)
			}
			names = append(names, name)
			continue
		}
		var entry struct {
			Name  string     `json:"name"`
			Hooks EventHooks `json:"hooks"`
		}
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&entry); err != nil {
			return nil, fmt.Errorf("tools[%d]: a tool is a name or {name, hooks}: %w", i, err)
		}
		if entry.Name == "" {
			return nil, fmt.Errorf("tools[%d]: a tool entry with hooks needs a name", i)
		}
		names = append(names, entry.Name)
		if lifted == nil {
			lifted = ToolHooks{}
		}
		for phase, entries := range entry.Hooks {
			if lifted[entry.Name] == nil {
				lifted[entry.Name] = EventHooks{}
			}
			lifted[entry.Name][phase] = append(lifted[entry.Name][phase], entries...)
		}
	}
	if lifted == nil {
		return def, nil
	}
	var existing ToolHooks
	if th, ok := top["tool_hooks"]; ok {
		if err := json.Unmarshal(th, &existing); err != nil {
			return nil, fmt.Errorf("tool_hooks: %w", err)
		}
	}
	if existing == nil {
		existing = ToolHooks{}
	}
	for tool, events := range lifted {
		if existing[tool] == nil {
			existing[tool] = EventHooks{}
		}
		for phase, entries := range events {
			existing[tool][phase] = append(existing[tool][phase], entries...)
		}
	}
	var err error
	if top["tools"], err = json.Marshal(names); err != nil {
		return nil, err
	}
	if top["tool_hooks"], err = json.Marshal(existing); err != nil {
		return nil, err
	}
	return json.Marshal(top)
}

// Additions are the hooks a run adds to its agent's own: what the run request
// named, and what a parent run passed down. Only ever added — a run cannot
// remove or replace a hook its agent's definition carries. They are entries,
// not resolved hooks, so a sub-agent resolves what it inherits in its own run
// and a resumed run resolves what its record kept.
type Additions struct {
	Hooks     EventHooks `json:"hooks,omitempty"`
	ToolHooks ToolHooks  `json:"tool_hooks,omitempty"`
}

// Empty reports whether nothing is added.
func (a Additions) Empty() bool { return len(a.Hooks) == 0 && len(a.ToolHooks) == 0 }

// Merge returns a's entries followed by b's, per event and per tool.
func (a Additions) Merge(b Additions) Additions {
	if b.Empty() {
		return a
	}
	if a.Empty() {
		return b
	}
	out := Additions{Hooks: EventHooks{}, ToolHooks: ToolHooks{}}
	for _, src := range []Additions{a, b} {
		for phase, entries := range src.Hooks {
			out.Hooks[phase] = append(out.Hooks[phase], entries...)
		}
		for tool, events := range src.ToolHooks {
			if out.ToolHooks[tool] == nil {
				out.ToolHooks[tool] = EventHooks{}
			}
			for phase, entries := range events {
				out.ToolHooks[tool][phase] = append(out.ToolHooks[tool][phase], entries...)
			}
		}
	}
	return out
}

// Validate checks the additions' shape, and that each tool's hooks name a tool
// the agent has: a hook on a tool the run cannot call would never fire, and a
// caller who added one believes a gate is there.
func (a Additions) Validate(agentTools []string) error {
	if err := a.Hooks.Validate(""); err != nil {
		return err
	}
	if err := a.ToolHooks.Validate(); err != nil {
		return err
	}
	for tool := range a.ToolHooks {
		found := false
		for _, t := range agentTools {
			if t == tool {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("tool_hooks: %s is not one of the agent's tools", tool)
		}
	}
	return nil
}

type additionsKey struct{}

// WithAdditions records the additions a run carries, so the sub-agents it
// starts inherit them.
func WithAdditions(ctx context.Context, a Additions) context.Context {
	return context.WithValue(ctx, additionsKey{}, a)
}

// AdditionsFrom returns the additions of the run ctx belongs to.
func AdditionsFrom(ctx context.Context) Additions {
	a, _ := ctx.Value(additionsKey{}).(Additions)
	return a
}
