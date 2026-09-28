package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// withRunHooks resolves the hooks a run carries and attaches them to the run's
// ctx, where every hook the run fires is taken from: its AgentDef's, verbatim,
// then what the run adds — what a parent run passed down (on ctx) followed by
// what this run's request named. Additions are only ever appended; nothing a
// run is given can remove a hook its agent carries. The run's additions are put
// back on ctx for the sub-agents it starts.
//
// The set is also kept by run id for the hooks fired outside the loop (a manual
// compaction, run_end). A set that fails to resolve still attaches: the loop
// refuses to start a run whose hooks it could not resolve, because a gate a
// definition or a caller named must not quietly be missing.
func (s *Server) withRunHooks(ctx context.Context, runID, agentName string, def config.AgentDef, requested hooks.Additions) context.Context {
	added := hooks.AdditionsFrom(ctx).Merge(requested)
	ctx = hooks.WithAdditions(ctx, added)
	if len(def.Hooks) == 0 && len(def.ToolHooks) == 0 && added.Empty() {
		// No hooks. A sub-agent's ctx still carries its parent's set, which is
		// the parent's own gates, not the child's: replace it with an empty one.
		if hooks.SetFrom(ctx) != nil {
			return hooks.WithSet(ctx, hooks.NewSet())
		}
		return ctx
	}
	pins := &pinRecorder{lookup: builtin.HookDefLookup(s.store), defs: map[string]string{}}
	set := s.resolveRunHooks(ctx, agentName, def, requested, added, pins.record)
	if runID != "" {
		if set.Err() == nil {
			if err := s.recordPinnedHooks(ctx, runID, &pinnedHooks{Agent: agentHooksFingerprint(def), Defs: pins.defs}); err != nil {
				// Unrecorded, a resumed run would re-resolve its hooks — the
				// thing the record exists to prevent — so the run does not start.
				set = hooks.FailedSet(fmt.Errorf("record the run's hooks: %w", err))
			}
		}
		s.runHookSets.Store(runID, set)
	}
	return hooks.WithSet(ctx, set)
}

// withResumedRunHooks is withRunHooks for a run resumed after a pause: it
// fires the hooks the run started with, not what its definitions say now. The
// agent's own hooks must be the ones it started with (their fingerprint), and
// every HookDef reference resolves to the version it found then — a version
// since deleted, or an agent whose hooks were changed, stops the run rather
// than letting it continue under hooks it did not start with. A run recorded
// before hooks were pinned has nothing to replay and resolves as it always has.
func (s *Server) withResumedRunHooks(ctx context.Context, run store.Run, def config.AgentDef, pinned *pinnedHooks) context.Context {
	if pinned == nil {
		return s.withRunHooks(ctx, run.ID, run.Agent, def, hooks.Additions{})
	}
	var set *hooks.Set
	if agentHooksFingerprint(def) != pinned.Agent {
		set = hooks.FailedSet(fmt.Errorf("agent %s: its hooks were changed while the run was paused; a run does not continue under hooks it did not start with", run.Agent))
	} else {
		set = s.resolveRunHooks(ctx, run.Agent, def, hooks.Additions{}, hooks.AdditionsFrom(ctx), pinnedLookup(s.store, pinned.Defs))
	}
	s.runHookSets.Store(run.ID, set)
	return hooks.WithSet(ctx, set)
}

func (s *Server) resolveRunHooks(ctx context.Context, agentName string, def config.AgentDef, requested, added hooks.Additions, lookup hooks.LookupDef) *hooks.Set {
	// A caller's own additions must name tools this agent has; inherited ones
	// were checked against the parent's, and a child without the tool simply
	// never fires them.
	if err := requested.Validate(def.Tools); err != nil {
		return hooks.FailedSet(fmt.Errorf("the run's hooks: %w", err))
	}
	if err := requested.RefuseCredentials(); err != nil {
		return hooks.FailedSet(fmt.Errorf("the run's hooks: %w", err))
	}
	set := hooks.NewSet()
	// The additions are registered FIRST, so they sit outside the agent's own
	// hooks: a pre chain runs in listed order, so the agent's gates decide on
	// the input an added hook may have rewritten (not on the model's, which
	// the addition could then replace unchecked); a post chain runs reversed,
	// so the agent's hooks — a redaction, say — act on the result before any
	// added hook sees it.
	//
	// A run's additions are the caller's, not an operator's definition: they
	// resolve in the run's tenant and never widen hosts.
	run := hooks.Source{Owner: "run", Tenant: tools.RunIdentity(ctx).TenantID}
	if err := hooks.Resolve(ctx, run, added.Hooks, added.ToolHooks, lookup, s.hookPermits, set); err != nil {
		return hooks.FailedSet(fmt.Errorf("the run's hooks: %w", err))
	}
	// What a definition added (a TeamDef state's hooks) resolves as that
	// definition's: in its tenant, and able to widen hosts when an operator
	// wrote it and the permit list names the hook. After the caller's, which
	// keeps the order they were added in: a caller's additions come from a
	// run request, at the top of the tree, before any team adds its own.
	for _, g := range added.Sourced {
		if err := hooks.Resolve(ctx, g.Source, g.Hooks, g.ToolHooks, lookup, s.hookPermits, set); err != nil {
			return hooks.FailedSet(fmt.Errorf("%s's hooks: %w", g.Source.Owner, err))
		}
	}
	agent := hooks.Source{Owner: "agent:" + agentName, Tenant: def.OwnerTenant, OperatorAuthored: def.OperatorAuthored}
	if err := hooks.Resolve(ctx, agent, def.Hooks, def.ToolHooks, lookup, s.hookPermits, set); err != nil {
		return hooks.FailedSet(fmt.Errorf("agent %s: %w", agentName, err))
	}
	return set
}

// additionsRecord is what a run's record keeps of a caller's additions (nil =
// none). A definition's (Sourced) are kept under their own key, with their
// source; see runConfigRecord.SourcedHooks.
func additionsRecord(a hooks.Additions) *hooks.Additions {
	caller := hooks.Additions{Hooks: a.Hooks, ToolHooks: a.ToolHooks}
	if caller.Empty() {
		return nil
	}
	return &caller
}

// runHookSet is the resolved set of a live run, or nil.
func (s *Server) runHookSet(runID string) *hooks.Set {
	if v, ok := s.runHookSets.Load(runID); ok {
		return v.(*hooks.Set)
	}
	return nil
}

// pinnedHooks is what a run's hooks resolved to when it started, kept in its
// run_config so a resumed run fires exactly those.
//
// It records identities, never content: run_config is returned as the run's
// spec to whoever may read the run, and an agent's inline webhooks (a URL may
// carry a token, a header a credential) are not theirs to read. So the agent's
// own hook entries are kept as a fingerprint — resume refuses a run whose
// agent's hooks changed — and each HookDef lookup as the def_id it found, which
// is content-addressed and so pins the body too.
type pinnedHooks struct {
	// Agent is agentHooksFingerprint of the agent the run started with.
	Agent string `json:"agent"`
	// Defs maps every HookDef lookup the resolution made, "tenant/name@version"
	// (version 0 = the active one), to the def_id it found, or "" where it
	// found none — so a HookDef created later in the run's own tenant cannot
	// take the place of the shared one the run fell back to.
	Defs map[string]string `json:"defs,omitempty"`
}

func pinKey(tenant, name string, version int) string {
	return fmt.Sprintf("%s/%s@%d", tenant, name, version)
}

// agentHooksFingerprint identifies an agent's own hooks and what decides how
// they resolve: its tenant and whether an operator wrote it.
func agentHooksFingerprint(def config.AgentDef) string {
	b, _ := json.Marshal(struct {
		Hooks            hooks.EventHooks `json:"hooks,omitempty"`
		ToolHooks        hooks.ToolHooks  `json:"tool_hooks,omitempty"`
		OwnerTenant      string           `json:"owner_tenant,omitempty"`
		OperatorAuthored bool             `json:"operator_authored,omitempty"`
	}{def.Hooks, def.ToolHooks, def.OwnerTenant, def.OperatorAuthored})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pinRecorder is a HookDef lookup that remembers every answer it gave.
type pinRecorder struct {
	lookup hooks.LookupDef
	defs   map[string]string
}

func (p *pinRecorder) record(ctx context.Context, tenant, name string, version int) (hooks.Def, string, error) {
	def, id, err := p.lookup(ctx, tenant, name, version)
	switch {
	case err == nil:
		p.defs[pinKey(tenant, name, version)] = id
	case hooks.IsDefNotFound(err):
		p.defs[pinKey(tenant, name, version)] = ""
	}
	return def, id, err
}

// pinnedLookup answers each lookup as it was answered when the run started.
// A pinned version reads by def_id, whether or not it has since been retired;
// one since deleted stops the run. A lookup the run never made is refused
// rather than answered from today's definitions.
func pinnedLookup(st store.Store, defs map[string]string) hooks.LookupDef {
	return func(ctx context.Context, tenant, name string, version int) (hooks.Def, string, error) {
		id, ok := defs[pinKey(tenant, name, version)]
		switch {
		case !ok:
			return hooks.Def{}, "", fmt.Errorf("HookDef %s was not among the hooks the run started with", name)
		case id == "":
			return hooks.Def{}, "", hooks.DefNotFound(fmt.Errorf("no HookDef %q", name))
		case st == nil:
			return hooks.Def{}, "", fmt.Errorf("no store: HookDefs are not available")
		}
		row, err := st.HookDefGet(ctx, id)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return hooks.Def{}, "", fmt.Errorf("HookDef %s (%s), which the run started with, no longer exists", name, id)
			}
			return hooks.Def{}, "", err
		}
		var def hooks.Def
		if err := json.Unmarshal(row.Definition, &def); err != nil {
			return hooks.Def{}, "", fmt.Errorf("HookDef %s: %w", name, err)
		}
		return def, row.DefID, nil
	}
}

// recordPinnedHooks adds the pins to the run's record, read and written back.
// Assumption: nothing else writes the record in the instant between the run's
// creation and this write. The other writer is a retune — an operator acting
// on a run it can already see running; one interleaved inside that window
// (before the run's first model call) could lose its change or the pins, and
// a run whose pins were lost resumes the way a run recorded before pinning
// does, by resolving its hooks again.
func (s *Server) recordPinnedHooks(ctx context.Context, runID string, p *pinnedHooks) error {
	if s.store == nil {
		return nil
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	rec, _ := decodeRunConfig(run.RunConfig)
	rec.PinnedHooks = p
	return s.store.SetRunConfig(ctx, runID, rec.marshal())
}
