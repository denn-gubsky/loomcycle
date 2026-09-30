package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A restore writes each active pointer only when a promote would: its def is
// on the target under the pointer's own tenant and name. The envelopes here
// are what a hand-edited snapshot can say — the source's pointers are planted
// through the unchecked snapshot writer, exactly as a restore would have
// planted them before the check.

// pointerKind is one definition kind with an active pointer, as a test drives it.
type pointerKind struct {
	section     string
	defsSection string
	seedDef     func(ctx context.Context, s store.Store, defID, tenantID, name string, body json.RawMessage) error
	seedPtr     func(ctx context.Context, s store.Store, tenantID, name, defID string) error
	// active is the def id GetActive serves for (tenant, name).
	active   func(ctx context.Context, s store.Store, tenantID, name string) (string, error)
	restored func(r RestoreResult) int
	// resolves is the kind's live lookup, when it has one: whether a run in
	// tenantID naming name gets a definition, and its body when it does.
	resolves func(ctx context.Context, s store.Store, tenantID, name string) (string, bool)
}

func pointerKinds() []pointerKind {
	now := time.Now()
	return []pointerKind{
		{
			section:     "agent_def_active",
			defsSection: "agent_defs",
			seedDef: func(ctx context.Context, s store.Store, id, tenant, name string, body json.RawMessage) error {
				_, err := s.SnapshotRestoreAgentDef(ctx, store.AgentDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, Definition: body, CreatedAt: now})
				return err
			},
			seedPtr: func(ctx context.Context, s store.Store, tenant, name, id string) error {
				_, err := s.SnapshotRestoreAgentDefActive(ctx, store.AgentDefActiveEntry{TenantID: tenant, Name: name, DefID: id, PromotedAt: now})
				return err
			},
			active: func(ctx context.Context, s store.Store, tenant, name string) (string, error) {
				r, err := s.AgentDefGetActive(ctx, tenant, name)
				return r.DefID, err
			},
			restored: func(r RestoreResult) int { return r.AgentDefActiveRestored },
			resolves: func(ctx context.Context, s store.Store, tenant, name string) (string, bool) {
				def, ok := lookup.Agent(ctx, s, nil, tenant, name)
				return def.SystemPrompt, ok
			},
		},
		{
			section:     "skill_def_active",
			defsSection: "skill_defs",
			seedDef: func(ctx context.Context, s store.Store, id, tenant, name string, body json.RawMessage) error {
				_, err := s.SnapshotRestoreSkillDef(ctx, store.SkillDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, Definition: body, CreatedAt: now})
				return err
			},
			seedPtr: func(ctx context.Context, s store.Store, tenant, name, id string) error {
				_, err := s.SnapshotRestoreSkillDefActive(ctx, store.SkillDefActiveEntry{TenantID: tenant, Name: name, DefID: id, PromotedAt: now})
				return err
			},
			active: func(ctx context.Context, s store.Store, tenant, name string) (string, error) {
				r, err := s.SkillDefGetActive(ctx, tenant, name)
				return r.DefID, err
			},
			restored: func(r RestoreResult) int { return r.SkillDefActiveRestored },
			resolves: func(ctx context.Context, s store.Store, tenant, name string) (string, bool) {
				res, ok := lookup.Skill(ctx, s, nil, tenant, name)
				return res.Body, ok
			},
		},
		{
			section:     "team_def_active",
			defsSection: "team_defs",
			seedDef: func(ctx context.Context, s store.Store, id, tenant, name string, body json.RawMessage) error {
				_, err := s.SnapshotRestoreTeamDef(ctx, store.TeamDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, Definition: body, CreatedAt: now})
				return err
			},
			seedPtr: func(ctx context.Context, s store.Store, tenant, name, id string) error {
				_, err := s.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{TenantID: tenant, Name: name, DefID: id, PromotedAt: now})
				return err
			},
			active: func(ctx context.Context, s store.Store, tenant, name string) (string, error) {
				r, err := s.TeamDefGetActive(ctx, tenant, name)
				return r.DefID, err
			},
			restored: func(r RestoreResult) int { return r.TeamDefActiveRestored },
		},
		{
			section:     "hook_def_active",
			defsSection: "hook_defs",
			seedDef: func(ctx context.Context, s store.Store, id, tenant, name string, body json.RawMessage) error {
				_, err := s.SnapshotRestoreHookDef(ctx, store.HookDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, Definition: body, CreatedAt: now})
				return err
			},
			seedPtr: func(ctx context.Context, s store.Store, tenant, name, id string) error {
				_, err := s.SnapshotRestoreHookDefActive(ctx, store.HookDefActiveEntry{TenantID: tenant, Name: name, DefID: id, PromotedAt: now})
				return err
			},
			active: func(ctx context.Context, s store.Store, tenant, name string) (string, error) {
				r, err := s.HookDefGetActive(ctx, tenant, name)
				return r.DefID, err
			},
			restored: func(r RestoreResult) int { return r.HookDefActiveRestored },
		},
		{
			section:     "mcp_server_def_active",
			defsSection: "mcp_server_defs",
			seedDef: func(ctx context.Context, s store.Store, id, tenant, name string, body json.RawMessage) error {
				_, err := s.SnapshotRestoreMCPServerDef(ctx, store.MCPServerDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, Definition: body, CreatedAt: now})
				return err
			},
			seedPtr: func(ctx context.Context, s store.Store, tenant, name, id string) error {
				_, err := s.SnapshotRestoreMCPServerDefActive(ctx, store.MCPServerDefActiveEntry{TenantID: tenant, Name: name, DefID: id, PromotedAt: now})
				return err
			},
			active: func(ctx context.Context, s store.Store, tenant, name string) (string, error) {
				r, err := s.MCPServerDefGetActive(ctx, tenant, name)
				return r.DefID, err
			},
			restored: func(r RestoreResult) int { return r.MCPServerDefActiveRestored },
		},
		{
			section:     "schedule_def_active",
			defsSection: "schedule_defs",
			seedDef: func(ctx context.Context, s store.Store, id, tenant, name string, body json.RawMessage) error {
				_, err := s.SnapshotRestoreScheduleDef(ctx, store.ScheduleDefRow{DefID: id, TenantID: tenant, Name: name, Version: 1, Definition: body, CreatedAt: now})
				return err
			},
			seedPtr: func(ctx context.Context, s store.Store, tenant, name, id string) error {
				_, err := s.SnapshotRestoreScheduleDefActive(ctx, store.ScheduleDefActiveEntry{TenantID: tenant, Name: name, DefID: id, PromotedAt: now})
				return err
			},
			active: func(ctx context.Context, s store.Store, tenant, name string) (string, error) {
				r, err := s.ScheduleDefGetActive(ctx, tenant, name)
				return r.DefID, err
			},
			restored: func(r RestoreResult) int { return r.ScheduleDefActiveRestored },
			resolves: func(ctx context.Context, s store.Store, tenant, name string) (string, bool) {
				sr, ok := lookup.Schedule(ctx, s, nil, tenant, name)
				return sr.Agent, ok
			},
		},
	}
}

// foreignMarker is what globex's definitions hold. It must never reach acme
// through a restored pointer, nor a restore warning.
const foreignMarker = "GLOBEX-PRIVATE-INSTRUCTIONS"

// pointerBody is a definition body every kind stores and the kinds with a live
// lookup parse: an agent reads system_prompt, a skill body, a schedule agent.
func pointerBody(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"system_prompt": text, "body": text, "agent": text,
		"schedule": "0 3 * * *", "enabled": false,
	})
	return b
}

// pointerCase is one planted pointer and whether a restore may write it.
type pointerCase struct {
	tenantID, name, defID string
	admitted              bool
}

func TestRestore_ActivePointerIsWrittenOnlyForItsOwnTenantsDef(t *testing.T) {
	ctx := context.Background()
	// The defs every kind carries: globex's gate (the foreign one), acme's
	// own, and one in the operator layer.
	defs := []struct{ defID, tenantID, name, text string }{
		{"pdf_foreign", "globex", "gate", foreignMarker},
		{"pdf_own", "acme", "own", "acme's own definition"},
		{"pdf_shared", "", "shared", "the operator layer's definition"},
		{"pdf_gone", "acme", "ghost", "a def the envelope loses"},
	}
	cases := []pointerCase{
		{"acme", "own", "pdf_own", true},        // a tenant's own def
		{"", "shared", "pdf_shared", true},      // the operator layer's own def
		{"acme", "gate", "pdf_foreign", false},  // another tenant's def
		{"acme", "ghost", "pdf_gone", false},    // a def on neither side (stripped below)
		{"acme", "alias", "pdf_own", false},     // own tenant, another name
		{"acme", "shared", "pdf_shared", false}, // a tenant pointer at an operator-layer def
		{"", "stolen", "pdf_own", false},        // the operator layer at a tenant's def
	}
	for _, b := range coverageBackends() {
		for _, k := range pointerKinds() {
			t.Run(b.name+"/"+k.section, func(t *testing.T) {
				// The source is sqlite whatever the target: its snapshot writer
				// holds the pointers a promote refuses, so Capture carries them.
				src, srcClose := newTestStore(t)
				defer srcClose()
				dst := b.open(t)

				for _, d := range defs {
					if err := k.seedDef(ctx, src, d.defID, d.tenantID, d.name, pointerBody(d.text)); err != nil {
						t.Fatalf("seed def %s: %v", d.defID, err)
					}
				}
				for _, c := range cases {
					if err := k.seedPtr(ctx, src, c.tenantID, c.name, c.defID); err != nil {
						t.Fatalf("plant pointer %s/%s: %v", c.tenantID, c.name, err)
					}
				}
				_, raw, err := Capture(ctx, src, CaptureOptions{})
				if err != nil {
					t.Fatalf("Capture: %v", err)
				}
				raw = withoutDef(t, raw, k.defsSection, "pdf_gone")

				res, err := Restore(ctx, dst, raw, RestoreOptions{})
				if err != nil {
					t.Fatalf("Restore: %v", err)
				}

				admitted, refused := 0, 0
				for _, c := range cases {
					where := qualifiedName(c.tenantID, c.name)
					got, err := k.active(ctx, dst, c.tenantID, c.name)
					if c.admitted {
						admitted++
						if err != nil || got != c.defID {
							t.Errorf("%s: active = %q, %v; want %s restored", where, got, err, c.defID)
						}
						continue
					}
					refused++
					var nf *store.ErrNotFound
					if !errors.As(err, &nf) {
						t.Errorf("%s: active = %q, %v; want no pointer written", where, got, err)
					}
					if !hasWarning(res, k.section+" "+where+":", "not restored", c.defID) {
						t.Errorf("%s: no warning naming the refused pointer; warnings: %v", where, res.Warnings)
					}
				}
				if got := k.restored(res); got != admitted {
					t.Errorf("%s restored = %d, want %d", k.section, got, admitted)
				}
				if res.ActivePointersRefused != refused {
					t.Errorf("active_pointers_refused = %d, want %d", res.ActivePointersRefused, refused)
				}
				if got := res.Counts()["active_pointers_refused"]; got != refused {
					t.Errorf("Counts()[active_pointers_refused] = %d, want %d", got, refused)
				}
				for _, w := range res.Warnings {
					if strings.Contains(w, foreignMarker) || strings.Contains(w, "globex") {
						t.Errorf("a warning says what the foreign def is or whose: %q", w)
					}
				}

				// What a run in acme naming "gate" is served: nothing, not globex's.
				if k.resolves != nil {
					if body, ok := k.resolves(ctx, dst, "acme", "gate"); ok || strings.Contains(body, foreignMarker) {
						t.Errorf("acme/gate resolves (ok=%v) to %q; want no definition", ok, body)
					}
					if body, ok := k.resolves(ctx, dst, "acme", "own"); !ok || body != "acme's own definition" {
						t.Errorf("acme/own resolves (ok=%v) to %q; want its own definition", ok, body)
					}
				}
			})
		}
	}
}

// withoutDef rewrites an envelope as one whose defs section lost defID while
// its pointer stayed — the source store's foreign key will not hold such a
// pointer, a hand-edited envelope can. Fails if there was nothing to strip.
func withoutDef(t *testing.T, raw []byte, section, defID string) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	sec := env["sections"].(map[string]any)[section].(map[string]any)
	var kept []any
	for _, e := range sec["entries"].([]any) {
		if e.(map[string]any)["def_id"] != defID {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(sec["entries"].([]any)) {
		t.Fatalf("section %s has no def %s to strip; the missing-def case would prove nothing", section, defID)
	}
	sec["entries"] = kept
	// The checksum covers the body; drop it as a pre-checksum snapshot would.
	delete(env, "checksum")
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
