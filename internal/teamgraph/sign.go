package teamgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// teamContent is the closed set of content-identifying fields hashed into a
// TeamDef's content_sha256. It is a WHITELIST: a Definition field is out of the
// hash unless it is listed here, which is what keeps presentation out for free.
// Excluded deliberately: the COLOR scheme and the canvas LAYOUT (presentation —
// recolouring or dragging a node must not fork a def's identity) and
// identity/tenant (operational), mirroring how `skills:` and tenant_id are
// excluded from AgentDef's hash, so two tenants forking the same workflow share
// a hash.
//
// Handler fields ride States and ARE hashed — including SystemPrompt and
// InputTemplate, so editing a node's role or its prompt forks the definition,
// which is the intended behaviour. Both carry omitempty, so a definition that
// omits them marshals byte-identically to one written before they existed and
// every recorded content_sha256 stays valid.
//
// DO NOT reorder these fields: json.Marshal emits them in declaration order and
// the resulting bytes are the hash input, so reordering would break every
// existing content_sha256.
type teamContent struct {
	Name          string       `json:"name"`
	Entry         string       `json:"entry"`
	MaxIterations int          `json:"max_iterations,omitempty"`
	States        []State      `json:"states"`
	Transitions   []Transition `json:"transitions"`
	// Channels is the team's channel ACL, and it IS content — unlike Colors and
	// Layout. It is AUTHORITY, and authority that can change without changing
	// the definition's identity is not auditable: a verify against a recorded
	// hash would pass while the workflow had quietly gained a channel.
	//
	// Added LAST, with omitempty, for the reason the comment above gives: a
	// definition that omits it marshals byte-identically to one written before
	// the field existed, so every recorded content_sha256 stays valid.
	Channels *TeamChannels `json:"channels,omitempty"`
}

// Sign returns "sha256:" + the lowercase-hex SHA-256 of a TeamDef's canonical
// content (name + graph, minus colours). Deterministic: equal content → equal
// hash. Mirrors skills.Sign / agents.Sign.
func Sign(name string, d Definition) string {
	buf, err := json.Marshal(teamContent{
		Name:          name,
		Entry:         d.Entry,
		MaxIterations: d.MaxIterations,
		States:        d.States,
		Transitions:   d.Transitions,
		Channels:      d.Channels,
	})
	if err != nil {
		buf = []byte("{}")
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}
