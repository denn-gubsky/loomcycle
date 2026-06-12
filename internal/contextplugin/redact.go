package contextplugin

import (
	"bytes"
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// redactPlugin scrubs secrets from the OUTBOUND request so the model never sees
// them. It reuses the F32 redact.Redactor — Tier-A exact env-value masking plus
// the Tier-B heuristic value patterns (Authorization / sk- / AKIA / xox / ghp_ /
// key=value). Deterministic + idempotent (a redacted string doesn't re-match)
// and copy-on-write (never mutates its input). Distinct from F32, which redacts
// the PERSISTED transcript; this redacts what is SENT.
type redactPlugin struct {
	r               *redact.Redactor
	redactToolInput bool
}

func newRedactPlugin(spec config.ContextPluginSpec, secrets map[string]string) (Plugin, error) {
	return &redactPlugin{
		r:               redact.New(secrets, true), // withPatterns = the Tier-B heuristics
		redactToolInput: spec.RedactToolInput != nil && *spec.RedactToolInput,
	}, nil
}

func (p *redactPlugin) Name() string { return "redact" }

func (p *redactPlugin) Transform(_ context.Context, system []providers.ContentBlock, msgs []providers.Message) (
	[]providers.ContentBlock, []providers.Message, error) {
	if p.r == nil || !p.r.Enabled() {
		return system, msgs, nil
	}
	outSys, _ := p.redactBlocks(system)
	return outSys, p.redactMessages(msgs), nil
}

// redactBlocks is copy-on-write: returns the input slice when nothing changed
// (strings are immutable, so sharing is safe), else a new slice with the
// redacted blocks replaced. `changed` reports whether a new slice was allocated.
func (p *redactPlugin) redactBlocks(in []providers.ContentBlock) ([]providers.ContentBlock, bool) {
	var out []providers.ContentBlock
	for i := range in {
		nb, ch := p.redactBlock(in[i])
		if ch && out == nil {
			out = make([]providers.ContentBlock, len(in))
			copy(out, in[:i])
		}
		if out != nil {
			out[i] = nb
		}
	}
	if out == nil {
		return in, false
	}
	return out, true
}

// redactBlock operates on a value copy of the block (so it never touches the
// caller's backing array) and preserves every other field — notably Cacheable.
func (p *redactPlugin) redactBlock(b providers.ContentBlock) (providers.ContentBlock, bool) {
	changed := false
	if b.Text != "" {
		if s := p.r.String(b.Text); s != b.Text {
			b.Text = s
			changed = true
		}
	}
	if p.redactToolInput && len(b.ToolInput) > 0 {
		if rb := p.r.Bytes(b.ToolInput); !bytes.Equal(rb, b.ToolInput) {
			b.ToolInput = rb
			changed = true
		}
	}
	return b, changed
}

// redactMessages is copy-on-write over the message slice; it redacts each
// message's content blocks (+ the DeepSeek Reasoning echo) without mutating the
// input.
func (p *redactPlugin) redactMessages(in []providers.Message) []providers.Message {
	var out []providers.Message
	for i := range in {
		m := in[i] // value copy of the Message struct
		nc, contentChanged := p.redactBlocks(m.Content)
		var nr string
		reasonChanged := false
		if m.Reasoning != "" {
			if nr = p.r.String(m.Reasoning); nr != m.Reasoning {
				reasonChanged = true
			}
		}
		if (contentChanged || reasonChanged) && out == nil {
			out = make([]providers.Message, len(in))
			copy(out, in[:i])
		}
		if out != nil {
			m.Content = nc
			if reasonChanged {
				m.Reasoning = nr
			}
			out[i] = m
		}
	}
	if out == nil {
		return in
	}
	return out
}
