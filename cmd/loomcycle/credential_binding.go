package main

import (
	"context"
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
// Its one rule: a credential the run HOLDS but may not use here — expired,
// restricted by allowed_urls, or stored but not openable (no KEK, a wrong or
// rotated KEK, a corrupt seal; credential.HeldButUnusable) — is an error,
// never a miss. A miss lets the caller fall back; a tenant's own key quietly
// replaced by the operator's would bill the operator for the tenant's work
// and hide from the tenant that its key is not being used.
//
// Only a store READ fault keeps the long-standing fail-soft posture: logged,
// and treated as a miss. A read that fails says nothing about whether the run
// holds a key at all, and this resolver runs on every LLM call of every run,
// so failing closed would turn a database blip into an outage for every
// tenant, including the ones that store no keys. Once a row is found, every
// way it can fail to open is the tenant's key, and fails closed.
type credentialBinding struct {
	engine *credential.Engine
	// register masks a resolved value against a downstream transcript echo.
	register func(string)
}

// resolve looks name up for the run on ctx. what labels the log line.
//
// It reads the store even with no KEK configured: a row stored before the
// key was removed is still the run's credential, and must be found to be
// refused. That is up to three point reads per call on a deployment
// without a KEK.
func (b credentialBinding) resolve(ctx context.Context, what, name string) (credential.Resolved, bool, error) {
	ri := tools.RunIdentity(ctx)
	// A run a trigger started without the right to act as its user never
	// takes that user's own key — a webhook's user can come from the delivery
	// (substituteForRun applies the same rule to $cred: references).
	userID := ri.UserID
	if ri.CredentialsNoUserScope {
		userID = ""
	}
	res, found, err := b.engine.Resolve(ctx, ri.TenantID, tools.AgentName(ctx), userID, name)
	if credential.HeldButUnusable(err) {
		return credential.Resolved{}, false, err
	}
	if err != nil {
		// A store read fault is NOT a legitimate miss — still fail soft
		// (see the type comment), but make it observable so a silent
		// fall-back regression is diagnosable.
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
// a provider or tool key by its env-var name (ANTHROPIC_API_KEY, …). One held
// but unusable (expired, URL-restricted, unopenable) fails the call
// (ErrCredentialUnusable) instead of letting the driver fall back to the
// operator's key.
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
// One held but unusable (expired, URL-restricted, unopenable) is an error,
// so the tool leaves the variable unset rather than letting a same-named
// host variable stand in for it.
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
// provider on the strength of the tenant's own key for env-var name? A key
// held but unusable (expired, URL-restricted, unopenable) is not keyable:
// the provider is skipped, and the run goes to one the tenant can key or is
// refused.
// Either way it never reaches the operator's key, which a restricted run may
// not use. That is a normal answer, not logged; only a fault is.
func (b credentialBinding) keyable(ctx context.Context, tenantID, agentName, userID, name string) bool {
	// As resolve: a key resolve would not hand this run is not one to route on.
	if tools.RunIdentity(ctx).CredentialsNoUserScope {
		userID = ""
	}
	ok, err := b.engine.HasKey(ctx, tenantID, agentName, userID, name)
	if credential.HeldButUnusable(err) {
		return false
	}
	if err != nil {
		log.Printf("credential: keyable check %q for tenant=%s: %v", name, tenantID, err)
		return false
	}
	return ok
}
