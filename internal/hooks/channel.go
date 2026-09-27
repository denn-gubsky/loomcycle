package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A channel's hooks decide on each message published to it before any reader
// sees it. Unlike every other phase they are not part of a run: the message
// waits in the store, and a worker drives the channel's chain one hook at a
// time (InvokeChannel), recording its progress between hooks so a restart
// resumes rather than repeats.

// ChannelHookCall is the payload a channel_publish hook receives: the message,
// and where it was published. It carries nothing of the publisher's run.
type ChannelHookCall struct {
	Phase Phase `json:"phase"`
	// Owner is "channel:<name>", the channel whose definition carries the hook.
	Owner    string `json:"owner"`
	HookName string `json:"hook_name"`
	Channel  string `json:"channel"`
	Scope    string `json:"scope"`
	ScopeID  string `json:"scope_id,omitempty"`
	// MessageID is the message's id; a hook called again for the same message
	// (a retry after it failed, or after a restart) sees the same id.
	MessageID   string    `json:"message_id"`
	PublishedAt time.Time `json:"published_at"`
	PublishedBy string    `json:"published_by,omitempty"`
	// Origin is "starter_sink" for a Starter's per-run result, which a drop
	// does not remove: it is delivered as an error result instead.
	Origin string `json:"origin,omitempty"`
	// Attempt counts the calls of this hook for this message, from 1.
	Attempt int `json:"attempt"`
	// Body is the message as the previous hook in the chain left it.
	Body json.RawMessage `json:"body"`
}

// Channel hook decisions.
const (
	ChannelRelease = "release"
	ChannelDrop    = "drop"
	ChannelHold    = "hold"
)

// ChannelHookResult is what a channel_publish hook returns. An empty response
// (or 204) releases the message as it is.
//   - "release" delivers the message, with UpdatedBody in place of its body
//     when set; the next hook in the chain sees the updated body.
//   - "drop" removes it; Reason is recorded.
//   - "hold" keeps it from every reader until a person decides.
type ChannelHookResult struct {
	Decision    string          `json:"decision,omitempty"`
	UpdatedBody json.RawMessage `json:"updated_body,omitempty"`
	Reason      string          `json:"reason,omitempty"`
}

// check refuses a result that does not say one thing, so a mistake in a hook
// is reported rather than read as a release. An updated_body of JSON null is
// read as no rewrite.
func (r *ChannelHookResult) check() error {
	if t := bytes.TrimSpace(r.UpdatedBody); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		r.UpdatedBody = nil
	}
	switch r.Decision {
	case "", ChannelRelease:
		r.Decision = ChannelRelease
		if r.UpdatedBody != nil && !json.Valid(r.UpdatedBody) {
			return fmt.Errorf("updated_body is not valid JSON")
		}
	case ChannelDrop, ChannelHold:
		if r.UpdatedBody != nil {
			return fmt.Errorf("updated_body goes with release; a %s delivers nothing", r.Decision)
		}
	default:
		return fmt.Errorf("decision %q does not apply to %s; it is %q, %q or %q", r.Decision, PhaseChannelPublish, ChannelRelease, ChannelDrop, ChannelHold)
	}
	return nil
}

// Kind is the decision as a run records it: "rewrite_body" for a release that
// changed the body.
func (r ChannelHookResult) Kind() string {
	if r.Decision == ChannelRelease && r.UpdatedBody != nil {
		return "rewrite_body"
	}
	return r.Decision
}

// ChannelOwner is the Owner of a channel's hooks.
func ChannelOwner(channel string) string { return "channel:" + channel }

// ValidateChannelHooks checks the shape of a channel's hooks: channel_publish
// is their only event, and each entry is well formed. Whether a referenced
// HookDef exists, and answers channel_publish, is checked where a store is at
// hand.
func ValidateChannelHooks(e EventHooks) error {
	for _, phase := range sortedPhases(e) {
		if !IsChannelPhase(phase) {
			return fmt.Errorf("hooks: a channel's hooks answer %s only, not %q", PhaseChannelPublish, phase)
		}
		for _, entry := range e[phase] {
			if err := entry.validate(); err != nil {
				return fmt.Errorf("hooks.%s: %w", phase, err)
			}
		}
	}
	return nil
}

// ResolveChannel appends a channel's hooks to set, in the order the channel
// lists them. src.Owner is the channel's (ChannelOwner) and src.Tenant the
// tenant whose definition carries them; a reference resolves there, then in
// the shared tenant. A reference that resolves nowhere, or to a HookDef of
// another event, fails the whole resolution. A channel hook never widens
// hosts.
func ResolveChannel(ctx context.Context, src Source, e EventHooks, lookup LookupDef, set *Set) error {
	if err := ValidateChannelHooks(e); err != nil {
		return err
	}
	return resolveEvents(ctx, src, "", e, lookup, Permits{}, set)
}

// InvokeChannel runs one channel_publish hook on one message and returns its
// decision. The worker drives the chain itself, a hook at a time, so that its
// progress is durable between hooks. An error means the hook was unavailable
// (unreachable, timed out, or answered something that is not a decision); the
// hook's FailMode says what that means for the message, and DecisionReason
// what to record.
func (d *Dispatcher) InvokeChannel(ctx context.Context, h *Hook, call ChannelHookCall) (ChannelHookResult, error) {
	if !IsChannelPhase(h.Phase) {
		return ChannelHookResult{}, fmt.Errorf("hook %s/%s answers %s, not %s", h.Owner, h.Name, h.Phase, PhaseChannelPublish)
	}
	if h.Timeout <= 0 {
		// Not built by a Set (which resolves it): a zero timeout would expire
		// the call before it was made.
		cp := *h
		cp.Timeout = timeoutFor(h)
		h = &cp
	}
	call.Phase, call.Owner, call.HookName = h.Phase, h.Owner, h.Name
	var res ChannelHookResult
	if err := d.invoke(ctx, h, &call, &res); err != nil {
		return ChannelHookResult{}, err
	}
	if err := res.check(); err != nil {
		return ChannelHookResult{}, err
	}
	res.Reason = strings.TrimSpace(res.Reason)
	return res, nil
}
