package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Def is a HookDef's definition: one hook, stored once and named wherever it
// is used. It carries everything a hook needs except who it is attached to —
// an AgentDef, a TeamDef or a run request names it, and the attachment says
// which agent and which run.
type Def struct {
	// Description is shown to whoever the hook denies, holds or rewrites,
	// including a tenant that did not write it. The body stays private.
	Description string    `json:"description,omitempty"`
	Event       Phase     `json:"event"`
	Match       *DefMatch `json:"match,omitempty"`
	Body        DefBody   `json:"body"`
	FailMode    FailMode  `json:"fail_mode,omitempty"`
	TimeoutMs   int       `json:"timeout_ms,omitempty"`
}

// DefMatch narrows a tool-event hook within whatever it is attached to.
type DefMatch struct {
	// Tools are exact names or trailing-* prefix globs; empty matches all.
	Tools []string `json:"tools,omitempty"`
}

// DefBody is a hook's body: code-js run in-process, or a webhook.
type DefBody struct {
	Kind string `json:"kind"`
	Code string `json:"code,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Body kinds.
const (
	BodyKindCode = "code-js"
	BodyKindHTTP = "http"
)

// MaxDescriptionBytes bounds a HookDef's description, which is shown on
// every call the hook stops.
const MaxDescriptionBytes = 4 << 10

// Normalize fills the defaults a HookDef is stored with, so two definitions
// that behave the same hash the same.
func (d *Def) Normalize() {
	if d.FailMode == "" {
		d.FailMode = FailOpen
	}
	if d.Match != nil && len(d.Match.Tools) == 0 {
		d.Match = nil
	}
	d.Description = strings.TrimSpace(d.Description)
}

// Validate refuses a definition the dispatcher could not run. It checks shape
// only: whether a code body compiles is the caller's to check, because the
// code-js runtime is not linked into this package.
func (d Def) Validate() error {
	switch {
	case IsToolPhase(d.Event):
	case isRunPhase(d.Event):
		if d.Match != nil && len(d.Match.Tools) > 0 {
			return fmt.Errorf("match.tools selects tool calls; a %s hook has no tool to match", d.Event)
		}
	case d.Event == "":
		return fmt.Errorf("event is required (one of %s)", phaseList)
	default:
		return fmt.Errorf("event %q is not one of %s", d.Event, phaseList)
	}
	switch d.Body.Kind {
	case BodyKindCode:
		if strings.TrimSpace(d.Body.Code) == "" {
			return fmt.Errorf("body.code is required for a %s body", BodyKindCode)
		}
		if d.Body.URL != "" {
			return fmt.Errorf("a %s body takes code, not url", BodyKindCode)
		}
		if len(d.Body.Code) > MaxCodeBytes {
			return fmt.Errorf("body.code is %d bytes; the limit is %d", len(d.Body.Code), MaxCodeBytes)
		}
	case BodyKindHTTP:
		if d.Body.Code != "" {
			return fmt.Errorf("an %s body takes url, not code", BodyKindHTTP)
		}
		if !strings.HasPrefix(d.Body.URL, "http://") && !strings.HasPrefix(d.Body.URL, "https://") {
			return fmt.Errorf("body.url must be http:// or https://")
		}
	case "":
		return fmt.Errorf("body.kind is required (%s or %s)", BodyKindCode, BodyKindHTTP)
	default:
		return fmt.Errorf("body.kind %q is not %s or %s", d.Body.Kind, BodyKindCode, BodyKindHTTP)
	}
	if d.FailMode != "" && d.FailMode != FailOpen && d.FailMode != FailClosed {
		return fmt.Errorf("fail_mode must be %q or %q", FailOpen, FailClosed)
	}
	if d.TimeoutMs < 0 {
		return fmt.Errorf("timeout_ms must be ≥ 0")
	}
	if len(d.Description) > MaxDescriptionBytes {
		return fmt.Errorf("description is %d bytes; the limit is %d", len(d.Description), MaxDescriptionBytes)
	}
	return nil
}

const phaseList = "pre, post, post_failure, agent_start, agent_stop, subagent_start, subagent_stop, pre_compact, post_compact, run_end"

func isRunPhase(p Phase) bool {
	switch p {
	case PhaseAgentStart, PhaseAgentStop, PhaseSubagentStart, PhaseSubagentStop,
		PhasePreCompact, PhasePostCompact, PhaseRunEnd:
		return true
	}
	return false
}

// ValidateDefName checks a HookDef name: segments of [A-Za-z0-9_-] joined by
// "/". A name is written into references as name@version and into the
// host-widen permit as [tenant:]name, so "@" and ":" can never be part of one.
func ValidateDefName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > 128 {
		return fmt.Errorf("name is %d bytes; the limit is 128", len(name))
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return fmt.Errorf("name %q has an empty segment (no leading, trailing or double slash)", name)
		}
		for _, r := range seg {
			ok := r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !ok {
				return fmt.Errorf("name %q has invalid character %q (allowed: A-Z a-z 0-9 _ - and / between segments)", name, r)
			}
		}
	}
	return nil
}

// SignDef is a HookDef's content hash: the name and the normalized
// definition. The tenant is operational identity, not content, so it is left
// out — two tenants writing the same hook get the same hash.
func SignDef(name string, d Def) string {
	d.Normalize()
	buf, err := json.Marshal(struct {
		Name string `json:"name"`
		Def  Def    `json:"def"`
	}{name, d})
	if err != nil {
		buf = []byte("{}")
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}
