package teamgraph

import (
	"fmt"
	"sort"
	"strings"
)

// RenderMermaid generates a Mermaid `stateDiagram-v2` from a Definition, with
// unsaturated state fills applied via `classDef` (Mermaid renders node
// backgrounds natively). Transition edges carry their `on` label; per-edge
// colour is limited in stateDiagram-v2, so the label conveys the kind and a
// legend lists the colours (the Web UI panel + a future D2 output colour edges
// fully — see the RFC). If highlightState is non-empty and names a state, that
// state gets a bold outline (the running chunk's current state).
//
// Deterministic for a given (name, Definition, highlightState): states/edges are
// emitted in definition order; classDefs in first-appearance order.
func RenderMermaid(name string, d Definition, highlightState string) string {
	sc := Resolve(d)

	// outbound[state] = has ≥1 outgoing transition (to find final states).
	outbound := map[string]bool{}
	for _, t := range d.Transitions {
		outbound[t.From] = true
	}

	var b strings.Builder
	b.WriteString("stateDiagram-v2\n")
	if name != "" {
		fmt.Fprintf(&b, "  %%%% %s\n", mmSanitize(name)) // `%%` → a Mermaid comment line
	}
	if d.Entry != "" {
		fmt.Fprintf(&b, "  [*] --> %s\n", mmSanitize(d.Entry))
	}

	// Transitions, in definition order.
	for _, t := range d.Transitions {
		fmt.Fprintf(&b, "  %s --> %s: %s\n", mmSanitize(t.From), mmSanitize(t.To), mmSanitize(t.On))
	}

	// Final states → terminal marker (terminal handler, or no outbound edge).
	for _, s := range d.States {
		if s.Handler.Kind == HandlerTerminal || !outbound[s.ID] {
			fmt.Fprintf(&b, "  %s --> [*]\n", mmSanitize(s.ID))
		}
	}

	// Parallel-handler notes: surface the fan-out agents + consolidator.
	for _, s := range d.States {
		h := s.Handler
		if h.Kind == HandlerParallel {
			wait := h.Wait
			if wait == "" {
				wait = WaitAll
			}
			fmt.Fprintf(&b, "  note right of %s\n", mmSanitize(s.ID))
			fmt.Fprintf(&b, "    parallel: %s (wait: %s)\n", mmSanitize(strings.Join(h.Agents, ", ")), wait)
			if h.Consolidator != "" {
				fmt.Fprintf(&b, "    consolidator: %s\n", mmSanitize(h.Consolidator))
			}
			b.WriteString("  end note\n")
		}
	}

	// classDefs — one per distinct fill (first-appearance order) + the highlight
	// variant of the highlighted state's fill.
	writeClassDefs(&b, d, sc, highlightState)

	return b.String()
}

func writeClassDefs(b *strings.Builder, d Definition, sc Scheme, highlightState string) {
	// Distinct fills in first-appearance order → stable class names.
	var order []string
	seen := map[string]bool{}
	for _, s := range d.States {
		f := sc.Fill[s.ID]
		if f != "" && !seen[f] {
			seen[f] = true
			order = append(order, f)
		}
	}
	class := func(fill string) string { return "c" + sanitizeHex(fill) }

	for _, fill := range order {
		fmt.Fprintf(b, "  classDef %s fill:%s,color:#111,stroke:%s\n", class(fill), fill, strokeFor(d, sc, fill))
	}
	// Highlight class (fill of the highlighted state + a bold outline).
	hlValid := false
	if highlightState != "" {
		if f, ok := sc.Fill[highlightState]; ok && f != "" {
			fmt.Fprintf(b, "  classDef %s_hl fill:%s,color:#111,stroke:#111,stroke-width:3px\n", class(f), f)
			hlValid = true
		}
	}
	// Assign each state its class.
	for _, s := range d.States {
		f := sc.Fill[s.ID]
		if f == "" {
			continue
		}
		if hlValid && s.ID == highlightState {
			fmt.Fprintf(b, "  class %s %s_hl\n", mmSanitize(s.ID), class(f))
		} else {
			fmt.Fprintf(b, "  class %s %s\n", mmSanitize(s.ID), class(f))
		}
	}
}

// mmSanitize neutralises characters in a model/operator-authored id, label, or
// agent name that would break out of a Mermaid line (newline / carriage return)
// or forge a new statement/directive (`-->`, `%%`). State ids are sanitised
// identically wherever they appear (transitions, terminal markers, class lines)
// so the references still match. It is defence-in-depth against diagram
// injection — it does not reject the def (validation owns that).
func mmSanitize(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "-->", "__", "%%", "__").Replace(s)
}

// strokeFor picks a stroke for a fill's classDef: the accent of whichever state
// uses this fill (deterministic — lowest state id among users), so the outline
// is the saturated sibling of the fill.
func strokeFor(d Definition, sc Scheme, fill string) string {
	var users []string
	for _, s := range d.States {
		if sc.Fill[s.ID] == fill {
			users = append(users, s.ID)
		}
	}
	if len(users) == 0 {
		return fill
	}
	sort.Strings(users)
	if a := sc.Accent[users[0]]; a != "" {
		return a
	}
	return fill
}

// sanitizeHex turns "#99e9f2" into "99e9f2" (a valid classDef name suffix).
func sanitizeHex(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
