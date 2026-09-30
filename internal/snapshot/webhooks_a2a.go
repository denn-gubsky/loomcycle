package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The webhook and A2A sections (RFC DP §4.4, §4.10): webhook_defs,
// a2a_agent_defs and a2a_server_card_defs, each with its active pointers.
//
//   - Webhook defs lose their literal user_credentials values at capture,
//     exactly as schedules do (the same strip and force), and a def that lost
//     any is restored disabled with the capture_disabled marker. The receiver
//     answers a marked def with 404, and only a fork that re-supplies every
//     listed key clears it. Webhooks have no fire count.
//   - A2A defs carry no secret material: a peer's auth names a per-run
//     credential KEY and a card's security schemes are public descriptions.
//     They travel verbatim.
//   - Every body of these new sections goes through the authoring validator
//     the target injects (RestoreOptions.Validators) before it is written: a
//     peer endpoint is something the target will dial, and restoring never
//     widens what an author could have created there. A row whose validator
//     is not wired is skipped, never written unvalidated.
//
// The live row stands, as in every def section.

// captureWebhooks reads every tenant's webhook defs, stripping literal
// credentials, and every active pointer.
func captureWebhooks(ctx context.Context, s store.Store, defs *WebhookDefsSection, active *WebhookDefActiveSection) error {
	defs.Version = SectionVersion
	active.Version = SectionVersion
	rows, err := s.SnapshotReadWebhookDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot webhook_defs: %w", err)
	}
	defs.Entries = make([]WebhookDefEntry, 0, len(rows))
	for _, r := range rows {
		body, stripped, err := stripTriggerCredentials(r.Definition)
		if err != nil {
			// A body this cannot read is a body it cannot prove secret-free.
			return fmt.Errorf("snapshot webhook_defs %s: %w", r.DefID, err)
		}
		defs.Entries = append(defs.Entries, WebhookDefEntry{
			DefID: r.DefID, TenantID: r.TenantID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID,
			Definition: body, Description: r.Description, CreatedAt: r.CreatedAt.UTC(),
			CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
			Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic,
			StrippedCredentials: stripped,
		})
	}
	pointers, err := s.SnapshotReadWebhookDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot webhook_def_active: %w", err)
	}
	active.Entries = make([]WebhookDefActiveEntry, 0, len(pointers))
	for _, p := range pointers {
		active.Entries = append(active.Entries, WebhookDefActiveEntry{
			Name: p.Name, TenantID: p.TenantID, DefID: p.DefID, PromotedAt: p.PromotedAt.UTC(), PromotedByAgentID: p.PromotedByAgentID,
		})
	}
	return nil
}

// captureA2A reads every tenant's A2A peer and server-card defs and their
// active pointers, verbatim.
func captureA2A(ctx context.Context, s store.Store, sec *Sections) error {
	sec.A2AAgentDefs.Version = SectionVersion
	sec.A2AAgentDefActive.Version = SectionVersion
	sec.A2AServerCardDefs.Version = SectionVersion
	sec.A2AServerCardDefActive.Version = SectionVersion

	agents, err := s.SnapshotReadA2AAgentDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot a2a_agent_defs: %w", err)
	}
	sec.A2AAgentDefs.Entries = make([]A2AAgentDefEntry, 0, len(agents))
	for _, r := range agents {
		sec.A2AAgentDefs.Entries = append(sec.A2AAgentDefs.Entries, A2AAgentDefEntry{
			DefID: r.DefID, TenantID: r.TenantID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID,
			Definition: r.Definition, Description: r.Description, CreatedAt: r.CreatedAt.UTC(),
			CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
			Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic,
		})
	}
	agentPtrs, err := s.SnapshotReadA2AAgentDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot a2a_agent_def_active: %w", err)
	}
	sec.A2AAgentDefActive.Entries = make([]A2AAgentDefActiveEntry, 0, len(agentPtrs))
	for _, p := range agentPtrs {
		sec.A2AAgentDefActive.Entries = append(sec.A2AAgentDefActive.Entries, A2AAgentDefActiveEntry{
			Name: p.Name, TenantID: p.TenantID, DefID: p.DefID, PromotedAt: p.PromotedAt.UTC(), PromotedByAgentID: p.PromotedByAgentID,
		})
	}

	cards, err := s.SnapshotReadA2AServerCardDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot a2a_server_card_defs: %w", err)
	}
	sec.A2AServerCardDefs.Entries = make([]A2AServerCardDefEntry, 0, len(cards))
	for _, r := range cards {
		sec.A2AServerCardDefs.Entries = append(sec.A2AServerCardDefs.Entries, A2AServerCardDefEntry{
			DefID: r.DefID, TenantID: r.TenantID, Name: r.Name, Version: r.Version, ParentDefID: r.ParentDefID,
			Definition: r.Definition, Description: r.Description, CreatedAt: r.CreatedAt.UTC(),
			CreatedByAgentID: r.CreatedByAgentID, CreatedByRunID: r.CreatedByRunID,
			Retired: r.Retired, BootstrappedFromStatic: r.BootstrappedFromStatic,
		})
	}
	cardPtrs, err := s.SnapshotReadA2AServerCardDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot a2a_server_card_def_active: %w", err)
	}
	sec.A2AServerCardDefActive.Entries = make([]A2AServerCardDefActiveEntry, 0, len(cardPtrs))
	for _, p := range cardPtrs {
		sec.A2AServerCardDefActive.Entries = append(sec.A2AServerCardDefActive.Entries, A2AServerCardDefActiveEntry{
			Name: p.Name, TenantID: p.TenantID, DefID: p.DefID, PromotedAt: p.PromotedAt.UTC(), PromotedByAgentID: p.PromotedByAgentID,
		})
	}
	return nil
}

// validRestoredBody runs a new section's injected authoring validator over a
// body about to be written. A new section is never restored unvalidated: a
// missing validator skips the row, as a failing one does, with a warning.
func validRestoredBody(opts RestoreOptions, section, where string, body json.RawMessage, result *RestoreResult) bool {
	validate := opts.Validators[section]
	if validate == nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s: not restored: no %s validator is wired on this restore, and a definition is never restored unvalidated", where, section))
		return false
	}
	if err := validate(body); err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s: not restored: it fails the validation an author would face on this host: %v", where, err))
		return false
	}
	return true
}

// restoreWebhookDefs inserts each def the target does not have, in lineage
// order. A def it skips or refuses is not on the target, so the pointer pass
// refuses a pointer at it (admitActivePointer).
func restoreWebhookDefs(ctx context.Context, s store.Store, sec *WebhookDefsSection, opts RestoreOptions, scan *credScan, result *RestoreResult) {
	for _, e := range sec.Entries {
		where := fmt.Sprintf("webhook_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
		body := e.Definition
		stripped := len(e.StrippedCredentials) > 0
		if stripped {
			// Forced whatever the body says: a hand-edited envelope cannot
			// bring back a trigger that lost its credentials enabled.
			forced, err := forceCaptureDisabled(body, e.StrippedCredentials)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: it lost credentials and %v, so it cannot be stored disabled", where, err))
				continue
			}
			body = forced
		}
		if !validRestoredBody(opts, migrations.SectionWebhookDefs, where, body, result) {
			continue
		}
		inserted, err := s.SnapshotRestoreWebhookDef(ctx, store.WebhookDefRow{
			DefID: e.DefID, TenantID: e.TenantID, Name: e.Name, Version: e.Version, ParentDefID: e.ParentDefID,
			Definition: body, Description: e.Description, CreatedAt: e.CreatedAt,
			CreatedByAgentID: e.CreatedByAgentID, CreatedByRunID: e.CreatedByRunID,
			Retired: e.Retired, BootstrappedFromStatic: e.BootstrappedFromStatic,
		})
		if err != nil {
			// Typically a live def on the same (tenant, name, version) — this
			// instance's own yaml bootstrap, say. The live row stands.
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored, the live definition stands: %v", where, err))
			continue
		}
		if !inserted {
			continue // already here on this def_id: the live row stands
		}
		result.WebhookDefsRestored++
		if stripped {
			result.DefsDisabledForCredentials++
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: restored DISABLED because its literal user_credentials were not carried (keys: %s); "+
					"it answers every delivery with 404 until a WebhookDef fork supplies every one of them and sets enabled: true",
				where, strings.Join(e.StrippedCredentials, ", ")))
		}
		if !e.Retired {
			scan.add("webhook_def "+qualifiedName(e.TenantID, e.Name), e.TenantID, body)
		}
	}
}

// restoreActivePointer is the shared shape of the three pointer passes. The
// pointer is written only if the def it names is on the target under the
// pointer's own tenant and name — the promote's check (admitActivePointer),
// which also refuses a pointer at a def this restore skipped or refused — and
// a live pointer stands.
func restoreActivePointer(ctx context.Context, s store.Store, owner defOwner, section, tenantID, name, defID string, insert func() (bool, error), result *RestoreResult) bool {
	if !admitActivePointer(ctx, s, owner, section, tenantID, name, defID, result) {
		return false
	}
	inserted, err := insert()
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%s %s: %v", section, qualifiedName(tenantID, name), err))
		return false
	}
	return inserted
}

func restoreWebhookDefActive(ctx context.Context, s store.Store, sec *WebhookDefActiveSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		if restoreActivePointer(ctx, s, webhookDefOwner, migrations.SectionWebhookDefActive, e.TenantID, e.Name, e.DefID, func() (bool, error) {
			return s.SnapshotRestoreWebhookDefActive(ctx, store.WebhookDefActiveEntry{
				Name: e.Name, TenantID: e.TenantID, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
			})
		}, result) {
			result.WebhookDefActiveRestored++
		}
	}
}

func restoreA2AAgentDefs(ctx context.Context, s store.Store, sec *A2AAgentDefsSection, opts RestoreOptions, result *RestoreResult) {
	for _, e := range sec.Entries {
		where := fmt.Sprintf("a2a_agent_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
		if !validRestoredBody(opts, migrations.SectionA2AAgentDefs, where, e.Definition, result) {
			continue
		}
		inserted, err := s.SnapshotRestoreA2AAgentDef(ctx, store.A2AAgentDefRow{
			DefID: e.DefID, TenantID: e.TenantID, Name: e.Name, Version: e.Version, ParentDefID: e.ParentDefID,
			Definition: e.Definition, Description: e.Description, CreatedAt: e.CreatedAt,
			CreatedByAgentID: e.CreatedByAgentID, CreatedByRunID: e.CreatedByRunID,
			Retired: e.Retired, BootstrappedFromStatic: e.BootstrappedFromStatic,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored, the live definition stands: %v", where, err))
			continue
		}
		if inserted {
			result.A2AAgentDefsRestored++
		}
	}
}

func restoreA2AAgentDefActive(ctx context.Context, s store.Store, sec *A2AAgentDefActiveSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		if restoreActivePointer(ctx, s, a2aAgentDefOwner, migrations.SectionA2AAgentDefActive, e.TenantID, e.Name, e.DefID, func() (bool, error) {
			return s.SnapshotRestoreA2AAgentDefActive(ctx, store.A2AAgentDefActiveEntry{
				Name: e.Name, TenantID: e.TenantID, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
			})
		}, result) {
			result.A2AAgentDefActiveRestored++
		}
	}
}

func restoreA2AServerCardDefs(ctx context.Context, s store.Store, sec *A2AServerCardDefsSection, opts RestoreOptions, scan *credScan, result *RestoreResult) {
	for _, e := range sec.Entries {
		where := fmt.Sprintf("a2a_server_card_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
		if !validRestoredBody(opts, migrations.SectionA2AServerCardDefs, where, e.Definition, result) {
			continue
		}
		inserted, err := s.SnapshotRestoreA2AServerCardDef(ctx, store.A2AServerCardDefRow{
			DefID: e.DefID, TenantID: e.TenantID, Name: e.Name, Version: e.Version, ParentDefID: e.ParentDefID,
			Definition: e.Definition, Description: e.Description, CreatedAt: e.CreatedAt,
			CreatedByAgentID: e.CreatedByAgentID, CreatedByRunID: e.CreatedByRunID,
			Retired: e.Retired, BootstrappedFromStatic: e.BootstrappedFromStatic,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored, the live definition stands: %v", where, err))
			continue
		}
		if !inserted {
			continue
		}
		result.A2AServerCardDefsRestored++
		if !e.Retired {
			scan.add("a2a_server_card_def "+qualifiedName(e.TenantID, e.Name), e.TenantID, e.Definition)
		}
	}
}

// restoreA2AServerCardDefActive restores the card pointers and warns once per
// card it made live: a card publishes an AgentCard and accepts A2A calls for
// the agents it exposes as soon as the runtime resumes, so cloning a snapshot
// into a staging instance must not expose agents unnoticed.
func restoreA2AServerCardDefActive(ctx context.Context, s store.Store, sec *A2AServerCardDefActiveSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		if !restoreActivePointer(ctx, s, a2aServerCardDefOwner, migrations.SectionA2AServerCardDefActive, e.TenantID, e.Name, e.DefID, func() (bool, error) {
			return s.SnapshotRestoreA2AServerCardDefActive(ctx, store.A2AServerCardDefActiveEntry{
				Name: e.Name, TenantID: e.TenantID, DefID: e.DefID, PromotedAt: e.PromotedAt, PromotedByAgentID: e.PromotedByAgentID,
			})
		}, result) {
			continue
		}
		result.A2AServerCardDefActiveRestored++
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"a2a_server_card %s: restored ACTIVE; wherever the A2A server is enabled, it publishes an AgentCard and accepts "+
				"A2A calls for the agents it exposes once the runtime resumes (callers still need a token minted on this host)",
			qualifiedName(e.TenantID, e.Name)))
	}
}
