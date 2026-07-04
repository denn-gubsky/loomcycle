// sign.go — deterministic SHA-256 signing of an agent's content
// fields. Used by AgentDef set/fork to persist content_sha256 on
// every row, by the CLI `loomcycle hash agent <path>` subcommand
// for operator CI, and by the AgentDef `verify` op to answer "is
// this hash deployed?" without re-fetching the full Definition.
//
// The hash basis is documented + stable: same input bytes always
// produce the same hash, across loomcycle versions and platforms.
// The single source of truth for the algorithm is this file — the
// CLI helper and the in-process row writer both call Sign(), so
// the producer side and the verifier side never drift.
//
// What gets hashed (content-only — NOT metadata or identity):
//
//	name, description, system_prompt, tools, skills, model,
//	provider, tier, effort, sampling, max_tokens, max_iterations,
//	max_concurrent_children, code_body, providers, models, memory_scopes,
//	memory_quota_bytes, memory_backend, channels, evaluation_scopes,
//	interruption
//
// Explicitly excluded (would defeat the "did the content change?"
// question): def_id, version, parent_def_id, created_at,
// created_by_agent_id, created_by_run_id, retired,
// bootstrapped_from_static.
//
// Also excluded — the *tool-capability* ACLs that gate which substrate
// tools an agent may CALL but are NOT part of its authored definition:
//
//	agent_def_scopes, skill_def_scopes
//
// These are operator-yaml-only declarations resolved at boot; the in-DB
// agent_defs row never stores them, so hashing them would make a
// YAML-loaded agent and the same agent pushed via the substrate diverge.
//
// F14 (channels / evaluation_scopes / interruption): these three WERE
// excluded for the same round-trip reason, but they now DO round-trip
// through `AgentDef set` (mergedDef → agent_defs.definition JSONB) and
// through the MD loader (agents.Agent), so they are part of the hash on
// BOTH paths. The hash basis MUST match the closed set of fields that
// round-trip both paths — and these three now do. Empty values still
// omit (pointer/omitempty + normalize collapse), so every pre-F14 row
// without these fields hashes byte-identically.
//
// Canonical encoding rules:
//   - Go's encoding/json renders struct fields in declaration order
//     (stable across Go versions) and map keys in sorted order
//     (since Go 1.12) — these two properties give us a deterministic
//     byte sequence without an external JCS library.
//   - Empty slices and maps normalise to nil before encoding so the
//     `omitempty` tags collapse them out. An agent with `skills: []`
//     hashes identically to one with no `skills` key at all.
//   - system_prompt is trimmed of trailing whitespace; CR/LF line
//     endings normalise to LF. (Editor drift is the biggest unforced-
//     error source — operators who edit MDs in Windows-line-ending
//     editors would otherwise see spurious drift.)
//   - String slices preserve declaration order (semantic order
//     matters for tools, skills, providers).
//
// Output: "sha256:" + 64 lowercase hex chars. The prefix matches
// Docker's image-digest convention and leaves room for future algos
// (e.g. "sha512:" or "blake3:") without breaking parsers that split
// on the colon.
package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// AgentContent is the closed set of fields that participate in the
// content-hash. Field order matches the json tags' alphabetical
// order so the underlying JSON encoding is stable (Go encoding/json
// emits struct fields in declaration order; declaring them in tag-
// alphabetical order keeps the canonical form independent of any
// future field reordering for readability).
//
// Tag order matters; do NOT reorder without bumping the hash-format
// version on every existing row + re-running the backfill.
type AgentContent struct {
	// Channels is the Channel-tool ACL (F14). Pointer + omitempty so an
	// agent without a channels block omits the key entirely — an empty
	// AgentChannelACL is a VALUE struct that would serialise as
	// `"channels":{}` and change every pre-F14 row's hash; normalize()
	// collapses an all-empty pointer back to nil so both the
	// no-channels-agent and the substrate-read path (`"channels":{}` in the
	// persisted def) hash identically to before. Tag "channels" sorts
	// first (before code_body).
	Channels *AgentChannelACL `json:"channels,omitempty"`
	// CodeBody is the inline code-js orchestrator source (RFC J). Empty
	// for every LLM agent and for filesystem-backed static code agents
	// (whose body lives on disk, not in the definition) — so with
	// omitempty it serialises away and every pre-existing row hashes
	// byte-for-byte as before. NOT run through normalizeText: JS
	// whitespace/CRLF is semantically load-bearing and must match the
	// operator's `loomcycle hash agent` CI. Tag "code_body" sorts between
	// channels and compaction, preserving the alphabetical order.
	CodeBody string `json:"code_body,omitempty"`
	// Compaction is the per-agent context-compaction block (mirrors
	// config.Compaction; the agents package stays config-free). Content-
	// identifying — a fork that only changes a compaction field mints a distinct
	// content_sha256. Pointer + omitempty + normalize-collapse so a no-compaction
	// agent omits the key and hashes byte-identical to pre-feature rows. Tag
	// "compaction" sorts between code_body and description.
	Compaction  *Compaction `json:"compaction,omitempty"`
	Description string      `json:"description,omitempty"`
	Effort      string      `json:"effort,omitempty"`
	// EvaluationScopes / Interruption are the remaining interactive/
	// multi-agent ACL fields (F14). evaluation_scopes is a slice (nil →
	// omitted); interruption is a pointer for the same empty-struct reason
	// as Channels above. Tags sort between effort and max_concurrent_children.
	EvaluationScopes      []string                   `json:"evaluation_scopes,omitempty"`
	Interruption          *AgentInterruptionACL      `json:"interruption,omitempty"`
	MaxConcurrentChildren int                        `json:"max_concurrent_children,omitempty"`
	MaxIterations         int                        `json:"max_iterations,omitempty"`
	MaxTokens             int                        `json:"max_tokens,omitempty"`
	MemoryBackend         string                     `json:"memory_backend,omitempty"`
	MemoryQuotaBytes      int                        `json:"memory_quota_bytes,omitempty"`
	MemoryScopes          []string                   `json:"memory_scopes,omitempty"`
	Model                 string                     `json:"model,omitempty"`
	Models                map[string][]TierCandidate `json:"models,omitempty"`
	Name                  string                     `json:"name,omitempty"`
	Provider              string                     `json:"provider,omitempty"`
	Providers             []string                   `json:"providers,omitempty"`
	// Sampling is the per-agent LLM sampling block (mirrors config.Sampling;
	// the agents package stays config-free, so it's a local type). Content-
	// identifying: a fork that only changes temperature must mint a distinct
	// content_sha256. Pointer + omitempty so a no-sampling agent omits the key
	// and hashes byte-identical to pre-feature rows; normalize() collapses an
	// all-nil pointer back to nil. Tag "sampling" sorts between providers and
	// skills.
	Sampling     *Sampling `json:"sampling,omitempty"`
	Skills       []string  `json:"skills,omitempty"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	Tier         string    `json:"tier,omitempty"`
	// Tools is the agent's tool allowlist (the capability ceiling). Tag
	// "tools" sorts between tier and unbounded_iterations.
	Tools []string `json:"tools,omitempty"`
	// UnboundedIterations is content-identifying (like MaxIterations): a fork
	// that only flips it must get a distinct content_sha256, not silently
	// dedup (cf. F14). omitempty keeps pre-existing rows byte-identical.
	UnboundedIterations bool `json:"unbounded_iterations,omitempty"`
}

// Sign returns "sha256:" + the lowercase-hex SHA-256 of the canonical
// JSON encoding of c. Always returns a 71-character string
// ("sha256:" + 64 hex chars). Deterministic: equal AgentContent values
// always produce equal hashes; equal hashes imply equal canonical
// bytes (collision-resistant per SHA-256).
//
// Safe to call on a zero-value AgentContent — returns the hash of "{}".
func Sign(c AgentContent) string {
	normalize(&c)
	// Note: json.Marshal on a struct emits fields in declaration order,
	// and on a map emits keys in sorted order. Both are stable since
	// Go 1.12.
	buf, err := json.Marshal(c)
	if err != nil {
		// json.Marshal on a struct of basic types + string slices +
		// string maps cannot fail under normal conditions. If it ever
		// did, returning the zero-bytes hash is more useful than
		// panicking — callers can spot the sentinel and investigate.
		buf = []byte("{}")
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalize collapses zero-equivalent values so an agent declared
// with `skills: []` hashes identically to one with no `skills` key.
// Also normalises trailing whitespace + line endings on long string
// fields where editor drift would otherwise cause spurious mismatch.
func normalize(c *AgentContent) {
	// Empty slices → nil. The encoding/json behaviour difference between
	// []string{} (emits "[]") and []string(nil) (omitted via omitempty)
	// is the load-bearing detail here.
	if len(c.Tools) == 0 {
		c.Tools = nil
	}
	if len(c.MemoryScopes) == 0 {
		c.MemoryScopes = nil
	}
	if len(c.Providers) == 0 {
		c.Providers = nil
	}
	if len(c.Skills) == 0 {
		c.Skills = nil
	}
	if len(c.Models) == 0 {
		c.Models = nil
	}
	if len(c.EvaluationScopes) == 0 {
		c.EvaluationScopes = nil
	}
	// F14: collapse an all-empty channels/interruption pointer to nil so it
	// omits. CRITICAL for backward-compat + path convergence: a no-channels
	// agent (signFromMergedDef passes nil OR an empty struct) and the
	// substrate-read path (FromOverlay unmarshals `"channels":{}` from the
	// persisted def into a non-nil &{}) must both collapse to nil → no
	// "channels" key → byte-identical to every pre-F14 row. Only an agent
	// that actually sets publish/subscribe (or an interruption field)
	// contributes to the hash.
	if c.Channels != nil && len(c.Channels.Publish) == 0 && len(c.Channels.Subscribe) == 0 {
		c.Channels = nil
	}
	if c.Interruption != nil && !c.Interruption.Enabled && len(c.Interruption.Kinds) == 0 && c.Interruption.MaxPending == 0 {
		c.Interruption = nil
	}
	// Collapse an all-nil Sampling pointer to nil so a `"sampling":{}` (e.g. a
	// substrate read of a no-sampling def) hashes identically to a pre-feature
	// row. A meaningful temperature:0.0 is a non-nil *float64 and is preserved.
	if c.Sampling != nil && c.Sampling.Temperature == nil && c.Sampling.TopP == nil &&
		c.Sampling.TopK == nil && c.Sampling.FrequencyPenalty == nil && c.Sampling.PresencePenalty == nil &&
		c.Sampling.Seed == nil && len(c.Sampling.Stop) == 0 {
		c.Sampling = nil
	}
	// Same all-nil collapse for compaction (a substrate read of a no-compaction
	// def yields `"compaction":{}`) → hashes identically to a pre-feature row.
	if c.Compaction != nil && c.Compaction.Enabled == nil && c.Compaction.TargetPercentage == nil &&
		c.Compaction.KeepLastN == nil && c.Compaction.KeepFirst == nil &&
		c.Compaction.AutoCompactAtPct == nil && c.Compaction.Model == nil {
		c.Compaction = nil
	}

	// Trim + normalise the system_prompt to insulate the hash from
	// editor drift (Windows line endings, trailing blank lines).
	c.SystemPrompt = normalizeText(c.SystemPrompt)
}

// normalizeText converts CRLF to LF, drops a trailing CR, and trims
// trailing whitespace + newlines. Idempotent.
func normalizeText(s string) string {
	if s == "" {
		return s
	}
	// CRLF → LF, then a lone trailing CR → "".
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, " \t\n\r")
	return s
}

// FromYAMLAgent builds an AgentContent from a parsed *Agent (boot-time
// load + CLI `hash agent` subcommand path). F14: channels /
// evaluation_scopes / interruption now DO round-trip through `AgentDef
// set` (they live in mergedDef → the agent_defs.definition JSONB), so they
// are part of the hash on both paths. The *Scopes ACLs
// (agent_def_scopes / skill_def_scopes) and Path still do not round-trip
// and stay excluded — see the package doc. The channels/interruption
// pointers are nil when empty so a no-ACL agent hashes exactly as before
// (normalize() also collapses an all-empty pointer defensively).
func FromYAMLAgent(a *Agent) AgentContent {
	if a == nil {
		return AgentContent{}
	}
	c := AgentContent{
		Name:                  a.Name,
		Description:           a.Description,
		Provider:              a.Provider,
		Model:                 a.Model,
		CodeBody:              a.Code,
		Tier:                  a.Tier,
		Effort:                a.Effort,
		MaxTokens:             a.MaxTokens,
		MaxIterations:         a.MaxIterations,
		MaxConcurrentChildren: a.MaxConcurrentChildren,
		Tools:                 a.Tools,
		Skills:                a.Skills,
		SystemPrompt:          a.SystemPrompt,
		Providers:             a.Providers,
		Models:                a.Models,
		MemoryScopes:          a.MemoryScopes,
		MemoryQuotaBytes:      a.MemoryQuotaBytes,
		MemoryBackend:         a.MemoryBackend,
		EvaluationScopes:      a.EvaluationScopes,
	}
	if len(a.Channels.Publish) > 0 || len(a.Channels.Subscribe) > 0 {
		c.Channels = &AgentChannelACL{Publish: a.Channels.Publish, Subscribe: a.Channels.Subscribe}
	}
	if a.Interruption.Enabled || len(a.Interruption.Kinds) > 0 || a.Interruption.MaxPending != 0 {
		c.Interruption = &AgentInterruptionACL{Enabled: a.Interruption.Enabled, Kinds: a.Interruption.Kinds, MaxPending: a.Interruption.MaxPending}
	}
	return c
}

// FromOverlay parses a JSON overlay (the structured form `AgentDef set` /
// `AgentDef fork` passes over the wire) into an AgentContent. Unknown
// fields are silently dropped — the operator-supplied overlay shape
// MAY carry extra metadata the substrate doesn't recognise; including
// it in the hash would defeat forward-compatibility.
//
// The overlay's resolved-against-active-row form is what's persisted
// in the agent_defs.definition JSONB column, so the same FromOverlay
// call applied to the persisted definition produces the same hash.
func FromOverlay(definition json.RawMessage) (AgentContent, error) {
	var c AgentContent
	if len(definition) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(definition, &c); err != nil {
		return AgentContent{}, err
	}
	return c, nil
}
