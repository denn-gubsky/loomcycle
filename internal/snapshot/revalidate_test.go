package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The def sections snapshots carried before restore-side validation — agent,
// skill, team, hook, MCP server and channel defs — go through the validator
// the call site wires. A row it refuses is not written and its pointer is
// refused; with no validator wired, the section restores as it always has and
// says so once.

// existingDefKinds is each of those sections with a pointer, keyed by its defs
// section: how to read a def back and which counter counts its refusals.
var existingDefKinds = map[string]struct {
	owner   defOwner
	refused func(RestoreResult) int
}{
	migrations.SectionAgentDefs:     {agentDefOwner, func(r RestoreResult) int { return r.AgentDefsRefused }},
	migrations.SectionSkillDefs:     {skillDefOwner, func(r RestoreResult) int { return r.SkillDefsRefused }},
	migrations.SectionTeamDefs:      {teamDefOwner, func(r RestoreResult) int { return r.TeamDefsRefused }},
	migrations.SectionHookDefs:      {hookDefOwner, func(r RestoreResult) int { return r.HookDefsRefused }},
	migrations.SectionMCPServerDefs: {mcpServerDefOwner, func(r RestoreResult) int { return r.MCPServerDefsRefused }},
}

// refuseMarked stands in for an authoring validator: it refuses a body that
// carries the marker, with a reason naming a host as the real MCP validator's
// does.
func refuseMarked(body json.RawMessage) error {
	if strings.Contains(string(body), "refuse-me") {
		return errors.New(`host "evil.example.net" not in LOOMCYCLE_HTTP_HOST_ALLOWLIST`)
	}
	return nil
}

// A row its section's validator refuses is not restored, its pointer is
// refused, and the rest of the section lands — for each older def section.
func TestRestore_ExistingDefSectionSkipsARowItsValidatorRefuses(t *testing.T) {
	ctx := context.Background()
	kinds := 0
	for _, k := range pointerKinds() {
		ex, ok := existingDefKinds[k.defsSection]
		if !ok {
			continue
		}
		kinds++
		t.Run(k.defsSection, func(t *testing.T) {
			src, srcClose := newTestStore(t)
			defer srcClose()
			dst, dstClose := newTestStore(t)
			defer dstClose()
			for _, d := range []struct{ id, name, body string }{
				{"d_ok", "fine", `{"marker":"fine"}`},
				{"d_bad", "crafted", `{"marker":"refuse-me"}`},
			} {
				if err := k.seedDef(ctx, src, d.id, "acme", d.name, json.RawMessage(d.body)); err != nil {
					t.Fatal(err)
				}
				if err := k.seedPtr(ctx, src, "acme", d.name, d.id); err != nil {
					t.Fatal(err)
				}
			}
			validators := passValidators()
			validators[k.defsSection] = refuseMarked
			res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: validators})

			if _, _, err := ex.owner(ctx, dst, "d_ok"); err != nil {
				t.Errorf("the valid def was not restored: %v (warnings %v)", err, res.Warnings)
			}
			if id, err := k.active(ctx, dst, "acme", "fine"); err != nil || id != "d_ok" {
				t.Errorf("the valid def's pointer = %q, %v; want d_ok", id, err)
			}
			var nf *store.ErrNotFound
			if _, _, err := ex.owner(ctx, dst, "d_bad"); !errors.As(err, &nf) {
				t.Errorf("the refused def is on the target (err = %v)", err)
			}
			if id, err := k.active(ctx, dst, "acme", "crafted"); !errors.As(err, &nf) {
				t.Errorf("the refused def's pointer was written: %q, %v", id, err)
			}
			if got := ex.refused(res); got != 1 {
				t.Errorf("%s refused = %d, want 1", k.defsSection, got)
			}
			if res.ActivePointersRefused != 1 {
				t.Errorf("active_pointers_refused = %d, want 1", res.ActivePointersRefused)
			}
			if !hasWarning(res, "acme/crafted", "d_bad", "not restored", "fails the validation", "evil.example.net") {
				t.Errorf("no warning names the refused def and the reason: %v", res.Warnings)
			}
			if !hasWarning(res, k.section, "acme/crafted", "not on this instance") {
				t.Errorf("no warning names the refused pointer: %v", res.Warnings)
			}
			if hasWarning(res, "without re-validation") {
				t.Errorf("a wired section warned it was not re-validated: %v", res.Warnings)
			}
		})
	}
	if kinds != len(existingDefKinds) {
		t.Fatalf("drove %d of the %d older def sections; pointerKinds lost one", kinds, len(existingDefKinds))
	}
}

// A channel's validator is given the whole entry — a channel row has no single
// body — and a channel it refuses is not restored.
func TestRestore_ChannelDefItsValidatorRefusesIsNotRestored(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	now := time.Now().UTC()
	for _, name := range []string{"fine", "crafted"} {
		if err := src.ChannelsCreate(ctx, store.ChannelRow{
			Name: name, TenantID: "acme", Scope: "tenant", Semantic: "queue", Publisher: "p", CreatedAt: now,
			Hooks: json.RawMessage(`{"channel_publish":["gate"]}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []ChannelDefEntry
	validators := passValidators()
	validators[migrations.SectionChannelDefs] = func(raw json.RawMessage) error {
		var e ChannelDefEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		seen = append(seen, e)
		if e.Name == "crafted" {
			return errors.New("hooks: header \"Host\" is set by the call itself")
		}
		return nil
	}
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: validators})

	if len(seen) != 2 || seen[0].TenantID != "acme" || seen[0].Publisher != "p" || !strings.Contains(string(seen[0].Hooks), "gate") {
		t.Fatalf("the validator saw %+v; want both entries with their tenant, publisher and hooks", seen)
	}
	if _, err := dst.ChannelGet(ctx, "acme", "fine"); err != nil {
		t.Errorf("the valid channel was not restored: %v", err)
	}
	var nf *store.ErrNotFound
	if _, err := dst.ChannelGet(ctx, "acme", "crafted"); !errors.As(err, &nf) {
		t.Errorf("the refused channel is on the target (err = %v)", err)
	}
	if res.ChannelDefsRestored != 1 || res.ChannelDefsRefused != 1 {
		t.Errorf("channel defs restored %d, refused %d; want 1 and 1", res.ChannelDefsRestored, res.ChannelDefsRefused)
	}
	if !hasWarning(res, "channel_def acme/crafted", "not restored", "is set by the call itself") {
		t.Errorf("no warning names the refused channel and the reason: %v", res.Warnings)
	}
}

// With no validator wired, an older def section restores every row as it did
// before, and one warning per section says it was not re-validated. A library
// caller that wires none does not lose definitions.
func TestRestore_ExistingDefSectionsWithoutValidatorsRestoreAsBefore(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	for _, k := range pointerKinds() {
		if _, ok := existingDefKinds[k.defsSection]; !ok {
			continue
		}
		for _, id := range []string{"d1", "d2"} {
			if err := k.seedDef(ctx, src, k.defsSection+"_"+id, "acme", k.defsSection+"-"+id, json.RawMessage(`{"marker":"refuse-me"}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "c", TenantID: "acme", Scope: "tenant", Semantic: "queue", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})

	for section, ex := range existingDefKinds {
		for _, id := range []string{"d1", "d2"} {
			if _, _, err := ex.owner(ctx, dst, section+"_"+id); err != nil {
				t.Errorf("%s %s was not restored with no validator wired: %v", section, id, err)
			}
		}
		if ex.refused(res) != 0 {
			t.Errorf("%s refused %d rows with no validator wired", section, ex.refused(res))
		}
		n := 0
		for _, w := range res.Warnings {
			if strings.HasPrefix(w, section+": ") && strings.Contains(w, "restored without re-validation") {
				n++
				if !strings.Contains(w, "2 row(s)") {
					t.Errorf("%s warning does not count its rows: %q", section, w)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d section warnings, want exactly 1 (%v)", section, n, res.Warnings)
		}
	}
	if res.AgentDefsRestored != 2 || res.SkillDefsRestored != 2 || res.TeamDefsRestored != 2 ||
		res.HookDefsRestored != 2 || res.MCPServerDefsRestored != 2 || res.ChannelDefsRestored != 1 {
		t.Errorf("restored counters = %+v, want every row", res.Counts())
	}
	if !hasWarning(res, "channel_defs: 1 row(s) restored without re-validation") {
		t.Errorf("no section warning for channel_defs: %v", res.Warnings)
	}

	// A section with no rows has nothing to re-validate and says nothing.
	empty, emptyClose := newTestStore(t)
	defer emptyClose()
	fresh, freshClose := newTestStore(t)
	defer freshClose()
	if again := mustRestore(t, fresh, mustCapture(t, empty), RestoreOptions{}); hasWarning(again, "without re-validation") {
		t.Errorf("an empty section warned: %v", again.Warnings)
	}
}

// An envelope written before any of this — schema 1, only the old def
// sections, no checksum, none of the newer sections — restores through the
// wired validators like a new one: the valid rows land, a crafted one is
// refused, and nothing complains about the sections it lacks.
func TestRestore_OldEnvelopeRevalidatesItsDefSections(t *testing.T) {
	ctx := context.Background()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	raw := []byte(`{"schema_version":1,"created_at":"2026-01-02T03:04:05Z","sections":{
	 "agent_defs":{"version":"1.0","entries":[
	   {"def_id":"ad_old","name":"helper","version":1,"definition":{"system_prompt":"hi"},"created_at":"2026-01-01T00:00:00Z"}]},
	 "agent_def_active":{"version":"1.0","entries":[
	   {"name":"helper","def_id":"ad_old","promoted_at":"2026-01-01T00:00:00Z"}]},
	 "mcp_server_defs":{"version":"1.0","entries":[
	   {"def_id":"md_ok","name":"peer","version":1,"definition":{"transport":"http","url":"https://mcp.example.com/mcp"},"created_at":"2026-01-01T00:00:00Z"},
	   {"def_id":"md_bad","name":"meta","version":1,"definition":{"transport":"http","url":"http://169.254.169.254/refuse-me"},"created_at":"2026-01-01T00:00:00Z"}]},
	 "mcp_server_def_active":{"version":"1.0","entries":[
	   {"name":"peer","def_id":"md_ok","promoted_at":"2026-01-01T00:00:00Z"},
	   {"name":"meta","def_id":"md_bad","promoted_at":"2026-01-01T00:00:00Z"}]}
	}}`)
	validators := passValidators()
	validators[migrations.SectionMCPServerDefs] = refuseMarked
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: validators})

	if res.AgentDefsRestored != 1 || res.AgentDefActiveRestored != 1 || res.MCPServerDefsRestored != 1 || res.MCPServerDefActiveRestored != 1 {
		t.Errorf("restored %v; want the agent, its pointer, the valid server and its pointer", res.Counts())
	}
	if res.MCPServerDefsRefused != 1 || res.ActivePointersRefused != 1 {
		t.Errorf("mcp_server_defs_refused = %d, active_pointers_refused = %d; want 1 and 1", res.MCPServerDefsRefused, res.ActivePointersRefused)
	}
	var nf *store.ErrNotFound
	if _, err := dst.MCPServerDefGet(ctx, "md_bad"); !errors.As(err, &nf) {
		t.Errorf("the crafted server is on the target (err = %v)", err)
	}
	for _, w := range res.Warnings {
		if !strings.Contains(w, "md_bad") && !strings.Contains(w, "meta") {
			t.Errorf("unexpected warning restoring an old envelope: %q", w)
		}
	}
}
