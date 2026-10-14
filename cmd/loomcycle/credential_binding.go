package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// credentialBinding resolves a run's own stored credential by NAME for the
// binders that put something else in its place when the run has none: the
// provider / tool key override (the operator's host key) and the Bash /
// Bashbox env injection (an operator-allowlisted host variable).
//
// Its one rule: a credential the run HOLDS but may not use (expired) is an
// error, never a miss. A miss lets the caller fall back; an expired tenant
// key quietly replaced by the operator's would bill the operator for the
// tenant's work and hide the lapse from the tenant.
//
// Other resolve faults (a store error, a restricted credential, a value that
// does not decrypt) keep the long-standing fail-soft posture: logged, and
// treated as a miss so a KEK gap never blocks a run.
type credentialBinding struct {
	engine *credential.Engine
	// register masks a resolved value against a downstream transcript echo.
	register func(string)
}

// resolve looks name up for the run on ctx. what labels the log line.
func (b credentialBinding) resolve(ctx context.Context, what, name string) (credential.Resolved, bool, error) {
	if !b.engine.CanResolve() {
		return credential.Resolved{}, false, nil
	}
	ri := tools.RunIdentity(ctx)
	res, found, err := b.engine.Resolve(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, name)
	if errors.Is(err, credential.ErrExpired) {
		return credential.Resolved{}, false, err
	}
	if err != nil {
		// A store/decrypt fault is NOT a legitimate miss — still fail soft
		// (never block a run on a KEK gap), but make it observable so a
		// silent fall-back regression is diagnosable.
		log.Printf("%s: credential resolve %q failed for tenant=%s: %v", what, name, ri.TenantID, err)
		return credential.Resolved{}, false, nil
	}
	if !found {
		return credential.Resolved{}, false, nil
	}
	if b.register != nil {
		b.register(res.Value)
	}
	return res, true, nil
}

// providerKey is the providers.CredentialResolver: a tenant/user override of
// a provider or tool key by its env-var name (ANTHROPIC_API_KEY, …). An
// expired one fails the call (ErrCredentialUnusable) instead of letting the
// driver fall back to the operator's key.
func (b credentialBinding) providerKey(ctx context.Context, name string) (providers.CredentialResolution, bool, error) {
	res, found, err := b.resolve(ctx, "credential", name)
	if err != nil {
		return providers.CredentialResolution{}, false, fmt.Errorf("%w: %w", providers.ErrCredentialUnusable, err)
	}
	if !found {
		return providers.CredentialResolution{}, false, nil
	}
	// RFC AV: carry the owning scope + scope_id so a driver can tag usage
	// (operator vs tenant/user spend) from the same resolve it uses for the key.
	return providers.CredentialResolution{Value: res.Value, Scope: res.Scope, ScopeID: res.ScopeID}, true, nil
}

// envVar resolves a credential injected into a Bash / Bashbox child's env.
// An expired one is an error, so the tool leaves the variable unset rather
// than letting a same-named host variable stand in for it.
func (b credentialBinding) envVar(what string) func(ctx context.Context, name string) (string, bool, error) {
	return func(ctx context.Context, name string) (string, bool, error) {
		res, found, err := b.resolve(ctx, what, name)
		if err != nil || !found {
			return "", false, err
		}
		return res.Value, true, nil
	}
}

// keyable is the RFC AX Layer-1 probe: may a restricted run be routed to a
// provider on the strength of the tenant's own key for env-var name? An
// expired key is not keyable, the same answer a URL-restricted one gets:
// the provider is skipped, and the run goes to one the tenant can key or is
// refused. Either way it never reaches the operator's key, which a
// restricted run may not use.
func (b credentialBinding) keyable(ctx context.Context, tenantID, agentName, userID, name string) bool {
	if !b.engine.CanResolve() {
		return false
	}
	ok, err := b.engine.HasKey(ctx, tenantID, agentName, userID, name)
	if err != nil {
		log.Printf("credential: keyable check %q for tenant=%s: %v", name, tenantID, err)
		return false
	}
	return ok
}
