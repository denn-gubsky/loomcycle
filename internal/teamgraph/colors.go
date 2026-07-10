package teamgraph

import "strings"

// The colour scheme (RFC AP): states/agents get UNSATURATED background fills;
// transitions get SATURATED edge colours. A def's optional `colors` block
// overrides the defaults below; colours are presentation only (excluded from the
// content hash).

// hue pairs a light (unsaturated) fill with a vivid (saturated) accent.
type hue struct{ fill, accent string }

// namedHues is the built-in palette. A colour value in a def may be one of these
// keys (resolved here) or a raw "#rrggbb" hex used verbatim.
var namedHues = map[string]hue{
	"cyan":   {"#99e9f2", "#1098ad"},
	"yellow": {"#ffec99", "#f08c00"},
	"orange": {"#ffd8a8", "#e8590c"},
	"blue":   {"#a5d8ff", "#1c7ed6"},
	"red":    {"#ffc9c9", "#e03131"},
	"pink":   {"#fcc2d7", "#e64980"},
	"violet": {"#d0bfff", "#7048e8"},
	"green":  {"#b2f2bb", "#2f9e44"},
}

// rotation is the deterministic order auto-assigned to states with no explicit
// colour and no keyword match, so every state stays visually distinct.
var rotation = []string{"cyan", "yellow", "orange", "blue", "red", "pink", "violet", "green"}

// keywordHue maps a role keyword to a hue. A state's fill defaults to the first
// keyword whose value prefixes any token (split on _ / -) of the state id, then
// of its handler agent name(s). Order matters (first match wins).
var keywordHue = []struct{ kw, hue string }{
	{"rfc", "cyan"},
	{"architect", "yellow"},
	{"plan", "orange"},
	{"implement", "blue"},
	{"code", "blue"},
	{"qa", "red"},
	{"review", "pink"},
	{"consolidat", "violet"},
	{"publish", "green"},
	{"ship", "green"},
	{"pr", "green"},
}

// Scheme is the resolved per-state colours for one Definition.
type Scheme struct {
	Fill   map[string]string // state id → unsaturated fill hex
	Accent map[string]string // state id → saturated accent hex (loomboard border/swatch)
}

// Resolve computes each state's fill + accent: explicit def override → role
// keyword → deterministic rotation. Deterministic for a given Definition.
func Resolve(d Definition) Scheme {
	sc := Scheme{Fill: map[string]string{}, Accent: map[string]string{}}
	auto := 0
	for _, s := range d.States {
		fill, accent, ok := "", "", false

		// 1. explicit override in the def's colors.states.
		if d.Colors != nil {
			if v, has := d.Colors.States[s.ID]; has && strings.TrimSpace(v) != "" {
				fill, accent, ok = resolveColorValue(v)
			}
		}
		// 2. role-keyword heuristic on the state id + handler agent name(s).
		if !ok {
			if h, found := keywordMatch(s); found {
				fill, accent, ok = namedHues[h].fill, namedHues[h].accent, true
			}
		}
		// 3. deterministic rotation for anything left over.
		if !ok {
			h := rotation[auto%len(rotation)]
			auto++
			fill, accent = namedHues[h].fill, namedHues[h].accent
		}
		sc.Fill[s.ID] = fill
		sc.Accent[s.ID] = accent
	}
	return sc
}

// EdgeColor returns the saturated colour for a transition's `on` label:
// explicit def override (exact label, then kind) → default (success green,
// conditional blue, pushback orange with a *qa* reason → red).
func EdgeColor(d Definition, on string) string {
	kind := on
	if i := strings.IndexByte(on, ':'); i >= 0 {
		kind = on[:i]
	}
	// Edges are saturated → use the accent shade (for a named key), or the raw
	// hex verbatim. An exact-label override wins over a kind override.
	if d.Colors != nil {
		if v, ok := d.Colors.Transitions[on]; ok && strings.TrimSpace(v) != "" {
			if _, accent, _ := resolveColorValue(v); accent != "" {
				return accent
			}
		}
		if v, ok := d.Colors.Transitions[kind]; ok && strings.TrimSpace(v) != "" {
			if _, accent, _ := resolveColorValue(v); accent != "" {
				return accent
			}
		}
	}
	switch kind {
	case OnSuccess:
		return namedHues["green"].accent
	case OnConditional:
		return namedHues["blue"].accent
	case OnPushback:
		if strings.Contains(strings.ToLower(on), "qa") {
			return namedHues["red"].accent
		}
		return namedHues["orange"].accent
	default:
		return namedHues["orange"].accent
	}
}

// resolveColorValue turns a def-supplied colour value into (fill, accent). A
// named palette key yields both shades; a raw hex is used for both (no derived
// accent). Returns ok=false only for the empty string.
func resolveColorValue(v string) (fill, accent string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", false
	}
	if h, named := namedHues[strings.ToLower(v)]; named {
		return h.fill, h.accent, true
	}
	return v, v, true // raw hex (or any literal) — used verbatim
}

func keywordMatch(s State) (string, bool) {
	toks := tokens(s.ID)
	toks = append(toks, tokens(s.Handler.Agent)...)
	for _, a := range s.Handler.Agents {
		toks = append(toks, tokens(a)...)
	}
	for _, kh := range keywordHue {
		for _, tok := range toks {
			if strings.HasPrefix(tok, kh.kw) {
				return kh.hue, true
			}
		}
	}
	return "", false
}

// tokens splits an id/name on _ / - and lowercases, so "peer_review" → [peer,
// review] and "qa-verify" → [qa, verify].
func tokens(s string) []string {
	if s == "" {
		return nil
	}
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '_' || r == '/' || r == '-'
	})
	return fields
}
