// Package promptinject is the system-prompt placeholder language: the closed
// {{memory:…}}, {{tool:…}}, {{document:…}} and ${var.…} families, the checks
// that refuse an unknown or malformed placeholder, and the expander that
// substitutes already-rendered bodies into a prompt.
//
// It is apart from internal/memory because it is a grammar, not storage: the
// config layer validates every agent's system prompt against it at load, and
// config sits below the memory services. Everything here is pure string work —
// the callers read the stores and hand the bodies in.
package promptinject
