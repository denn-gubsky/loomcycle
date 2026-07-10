package teamgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// teamContent is the closed set of content-identifying fields hashed into a
// TeamDef's content_sha256. The COLOR scheme (presentation) and identity/tenant
// (operational) are deliberately EXCLUDED — mirroring how `skills:` and
// tenant_id are excluded from AgentDef's hash — so two tenants forking the same
// workflow share a hash and recolouring never changes a def's identity.
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
	})
	if err != nil {
		buf = []byte("{}")
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}
