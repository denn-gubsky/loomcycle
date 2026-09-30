package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The schedule sections (RFC DP §4.3): schedule_defs, each entry carrying its
// def's run state, and schedule_def_active.
//
// Three decisions shape this file.
//
//   - Literal user_credentials values never travel. Capture removes them from
//     the carried body and records their KEYS as stripped_credentials; a def
//     that lost any is carried with enabled:false, so the envelope is safe
//     even against a restore that ignores the list. Restore forces
//     enabled:false again and writes a server-set capture_disabled marker into
//     the stored body, which the ScheduleDef fork honours: the def re-enables
//     only when a fork re-supplies every stripped key, and that fork inherits
//     the fire count. So no trigger fires without the credentials it was
//     authored with.
//   - The run state travels with its def and restores verbatim, fire_count
//     included, with insert-or-ignore: a re-restore can never reset a count,
//     and a def restored without its state would be dormant, not re-armed.
//   - Schedules restore ACTIVE. A restored schedule fires on the first sweep
//     after the runtime resumes. A snapshot is a copy, not a lease: if the
//     source keeps running, both instances fire, and together they can exceed
//     max_fires. That is documented, not prevented; restore says so once,
//     with the count of enabled schedules it brought back.
//
// The live row stands, as in every def section: a def, pointer or run state
// already on the target's key is left alone and not counted.

// triggerCredentialsKey and the other body keys the strip and force touch —
// schedule and webhook bodies spell them the same way. The strip
// and the force work from this explicit list and never re-project the body,
// because a projection through a struct could drop a field the struct does
// not know — a confinement bit among them.
const (
	triggerCredentialsKey     = "user_credentials"
	triggerEnabledKey         = "enabled"
	triggerCaptureDisabledKey = "capture_disabled"
)

// captureDisabledMarker is the stored shape of the capture_disabled marker;
// the ScheduleDef and WebhookDef tools, the scheduler and the webhook receiver
// decode the same keys (drift-tested there).
type captureDisabledMarker struct {
	StrippedCredentials []string `json:"stripped_credentials,omitempty"`
}

// captureSchedules reads every tenant's schedule defs with their run state,
// and every active pointer.
func captureSchedules(ctx context.Context, s store.Store, defs *ScheduleDefsSection, active *ScheduleDefActiveSection) error {
	defs.Version = SectionVersion
	active.Version = SectionVersion
	rows, err := s.SnapshotReadScheduleDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot schedule_defs: %w", err)
	}
	states, err := s.SnapshotReadScheduleRunState(ctx)
	if err != nil {
		return fmt.Errorf("snapshot schedule_run_state: %w", err)
	}
	stateByDef := make(map[string]store.ScheduleRunStateRow, len(states))
	for _, st := range states {
		stateByDef[st.DefID] = st
	}
	defs.Entries = make([]ScheduleDefEntry, 0, len(rows))
	for _, r := range rows {
		body, stripped, err := stripTriggerCredentials(r.Definition)
		if err != nil {
			// A body this cannot read is a body it cannot prove secret-free.
			return fmt.Errorf("snapshot schedule_defs %s: %w", r.DefID, err)
		}
		entry := ScheduleDefEntry{
			DefID:                  r.DefID,
			TenantID:               r.TenantID,
			Name:                   r.Name,
			Version:                r.Version,
			ParentDefID:            r.ParentDefID,
			Definition:             body,
			Description:            r.Description,
			CreatedAt:              r.CreatedAt.UTC(),
			CreatedByAgentID:       r.CreatedByAgentID,
			CreatedByRunID:         r.CreatedByRunID,
			Retired:                r.Retired,
			BootstrappedFromStatic: r.BootstrappedFromStatic,
			StrippedCredentials:    stripped,
		}
		if st, ok := stateByDef[r.DefID]; ok {
			entry.RunState = runStateEntry(st)
		}
		defs.Entries = append(defs.Entries, entry)
	}

	pointers, err := s.SnapshotReadScheduleDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot schedule_def_active: %w", err)
	}
	active.Entries = make([]ScheduleDefActiveEntry, 0, len(pointers))
	for _, p := range pointers {
		active.Entries = append(active.Entries, ScheduleDefActiveEntry{
			Name:              p.Name,
			TenantID:          p.TenantID,
			DefID:             p.DefID,
			PromotedAt:        p.PromotedAt.UTC(),
			PromotedByAgentID: p.PromotedByAgentID,
		})
	}
	return nil
}

// runStateEntry converts a run-state row. sqlite reads instants back in the
// host's zone; the envelope is portable, so it carries UTC.
func runStateEntry(st store.ScheduleRunStateRow) *ScheduleRunStateEntry {
	out := &ScheduleRunStateEntry{
		NextRunAt:  st.NextRunAt.UTC(),
		LastRunID:  st.LastRunID,
		LastStatus: st.LastStatus,
		LastError:  st.LastError,
		FireCount:  st.FireCount,
	}
	if !st.LastRunAt.IsZero() {
		t := st.LastRunAt.UTC()
		out.LastRunAt = &t
	}
	if !st.PausedUntil.IsZero() {
		t := st.PausedUntil.UTC()
		out.PausedUntil = &t
	}
	return out
}

// stripTriggerCredentials returns the body to carry and the credential keys
// it lost, sorted. A literal user_credentials value is removed; a value that
// is a reference ($cred:<name>, ${...}) is authored text, not a secret, and
// stays exactly as written — capture never expands one. When every value is
// stripped the user_credentials key goes too, so the envelope has no field
// for a per-user secret at all.
//
// A def already marked capture_disabled (restored here from an earlier
// snapshot and not yet re-enabled) keeps its marker in the body, and its
// keys are listed again, so the next hop restores it disabled as well.
//
// Any listed key means the body carries enabled:false. A body with nothing to
// strip and no marker is returned byte-for-byte.
func stripTriggerCredentials(body json.RawMessage) (json.RawMessage, []string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, nil, errors.New("definition is not a JSON object; its credentials cannot be checked")
	}

	keys := map[string]bool{}
	changed := false
	if raw, ok := m[triggerCredentialsKey]; ok && string(raw) != "null" {
		var creds map[string]any
		if err := json.Unmarshal(raw, &creds); err != nil {
			return nil, nil, fmt.Errorf("%s is not an object; its values cannot be checked", triggerCredentialsKey)
		}
		kept := map[string]any{}
		for k, v := range creds {
			if s, ok := v.(string); ok && (s == "" || isCredentialReference(s)) {
				kept[k] = v
				continue
			}
			keys[k] = true
		}
		if len(kept) != len(creds) {
			changed = true
			if len(kept) == 0 {
				delete(m, triggerCredentialsKey)
			} else {
				b, err := json.Marshal(kept)
				if err != nil {
					return nil, nil, err
				}
				m[triggerCredentialsKey] = b
			}
		}
	}
	marker, err := decodeCaptureDisabled(m)
	if err != nil {
		return nil, nil, err
	}
	for _, k := range marker {
		keys[k] = true
	}
	if len(keys) == 0 {
		return body, nil, nil
	}
	if !changed && enabledIsFalse(m) {
		return body, sortedKeys(keys), nil // an earlier hop's body, already disabled
	}
	m[triggerEnabledKey] = json.RawMessage("false")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return out, sortedKeys(keys), nil
}

// isCredentialReference reports whether a credential value is a reference
// rather than a literal: at least one $cred:/$ghapp:/${...} form, with nothing
// left over but an auth-scheme word ("Bearer ${LOOMCYCLE_X}"). The forms are
// the ones the header scanner treats as references.
func isCredentialReference(v string) bool {
	if !referenceRe.MatchString(v) {
		return false
	}
	for _, w := range strings.Fields(referenceRe.ReplaceAllString(v, "")) {
		if !authSchemes[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// decodeCaptureDisabled reads the keys of a capture_disabled marker already
// in a body.
func decodeCaptureDisabled(m map[string]json.RawMessage) ([]string, error) {
	raw, ok := m[triggerCaptureDisabledKey]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var marker captureDisabledMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return nil, fmt.Errorf("%s is malformed: %w", triggerCaptureDisabledKey, err)
	}
	return marker.StrippedCredentials, nil
}

func enabledIsFalse(m map[string]json.RawMessage) bool {
	var b *bool
	if raw, ok := m[triggerEnabledKey]; ok && json.Unmarshal(raw, &b) == nil && b != nil {
		return !*b
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// forceCaptureDisabled makes a body that lost credentials safe to store,
// whatever the envelope says: enabled:false, and the marker listing the keys
// a fork must re-supply. Every other field is kept as carried.
func forceCaptureDisabled(body json.RawMessage, stripped []string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, errors.New("definition is not a JSON object")
	}
	marker, err := json.Marshal(captureDisabledMarker{StrippedCredentials: stripped})
	if err != nil {
		return nil, err
	}
	m[triggerEnabledKey] = json.RawMessage("false")
	m[triggerCaptureDisabledKey] = marker
	return json.Marshal(m)
}

// scheduleFires reports whether a stored body would fire: not disabled, and
// no capture_disabled marker (the scheduler treats a marked def as disabled).
func scheduleFires(body json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return false
	}
	if raw, ok := m[triggerCaptureDisabledKey]; ok && string(raw) != "null" {
		return false
	}
	return !enabledIsFalse(m)
}

// restoredSchedule is what the active-pointer pass needs to know about a def
// from the same envelope: whether the pointer would make it fire.
type restoredSchedule struct {
	fires bool
}

// restoreScheduleDefs inserts each def the target does not have, then its run
// state. Entries are in lineage order (a fork after its parent), as captured.
// Returns what the active-pointer pass needs, keyed by def_id.
func restoreScheduleDefs(ctx context.Context, s store.Store, sec *ScheduleDefsSection, result *RestoreResult) map[string]restoredSchedule {
	out := make(map[string]restoredSchedule, len(sec.Entries))
	for _, e := range sec.Entries {
		where := fmt.Sprintf("schedule_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
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
		inserted, err := s.SnapshotRestoreScheduleDef(ctx, store.ScheduleDefRow{
			DefID:                  e.DefID,
			TenantID:               e.TenantID,
			Name:                   e.Name,
			Version:                e.Version,
			ParentDefID:            e.ParentDefID,
			Definition:             body,
			Description:            e.Description,
			CreatedAt:              e.CreatedAt,
			CreatedByAgentID:       e.CreatedByAgentID,
			CreatedByRunID:         e.CreatedByRunID,
			Retired:                e.Retired,
			BootstrappedFromStatic: e.BootstrappedFromStatic,
		})
		if err != nil {
			// Typically a live def on the same (tenant, name, version) — this
			// instance's own yaml bootstrap, say. The live row stands, and the
			// snapshot's run state has no def here to belong to.
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored, the live definition stands (its run state was not restored either): %v", where, err))
			continue
		}
		out[e.DefID] = restoredSchedule{fires: !e.Retired && scheduleFires(body)}
		if inserted {
			result.ScheduleDefsRestored++
			if stripped {
				result.DefsDisabledForCredentials++
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"%s: restored DISABLED because its literal user_credentials were not carried (keys: %s); "+
						"re-enable it with a ScheduleDef fork that supplies every one of them and enabled: true — the fork keeps the fire count",
					where, strings.Join(e.StrippedCredentials, ", ")))
			}
		}
		if e.RunState == nil {
			continue
		}
		// The def is on the target now (inserted, or already there with this
		// def_id); insert-or-ignore keeps a live run state and its count.
		st := store.ScheduleRunStateRow{
			DefID:      e.DefID,
			NextRunAt:  e.RunState.NextRunAt,
			LastRunID:  e.RunState.LastRunID,
			LastStatus: e.RunState.LastStatus,
			LastError:  e.RunState.LastError,
			FireCount:  e.RunState.FireCount,
		}
		if e.RunState.LastRunAt != nil {
			st.LastRunAt = *e.RunState.LastRunAt
		}
		if e.RunState.PausedUntil != nil {
			st.PausedUntil = *e.RunState.PausedUntil
		}
		stInserted, err := s.SnapshotRestoreScheduleRunState(ctx, st)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s run state: %v", where, err))
			continue
		}
		if stInserted {
			result.ScheduleRunStateRestored++
		}
	}
	return out
}

// restoreScheduleDefActive inserts each active pointer the target does not
// have, then says once how many enabled schedules the restore made live.
func restoreScheduleDefActive(ctx context.Context, s store.Store, sec *ScheduleDefActiveSection, defs map[string]restoredSchedule, result *RestoreResult) {
	enabled := 0
	for _, e := range sec.Entries {
		if !admitActivePointer(ctx, s, scheduleDefOwner, "schedule_def_active", e.TenantID, e.Name, e.DefID, result) {
			continue
		}
		inserted, err := s.SnapshotRestoreScheduleDefActive(ctx, store.ScheduleDefActiveEntry{
			Name:              e.Name,
			TenantID:          e.TenantID,
			DefID:             e.DefID,
			PromotedAt:        e.PromotedAt,
			PromotedByAgentID: e.PromotedByAgentID,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("schedule_def_active %s: %v", qualifiedName(e.TenantID, e.Name), err))
			continue
		}
		if !inserted {
			continue // the live pointer stands
		}
		result.ScheduleDefActiveRestored++
		if defs[e.DefID].fires {
			enabled++
		}
	}
	if enabled > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"schedules: %d enabled schedule(s) restored ACTIVE; each fires on the first sweep after resume. "+
				"A snapshot is a copy, not a lease: if the instance it was taken from is still running, both fire these "+
				"schedules and together can exceed max_fires — disable them on one side",
			enabled))
	}
}

// qualifiedName renders tenant/name, or name alone in the operator layer.
func qualifiedName(tenantID, name string) string {
	if tenantID == "" {
		return name
	}
	return tenantID + "/" + name
}
