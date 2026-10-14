package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// A forge sends every kind of event to one webhook URL and signs the body,
// not the header naming the event. A webhook definition that lists `events`
// has the receiver answer an event it does not list — and a ping — itself,
// before anything starts, and count an event only when the signed body has
// that event's shape. A `provider` preset is a named set of defaults for the
// headers, the auth kind and the shapes of the events it knows: a fourth forge
// needs no release, because every default can be written out instead.
//
// A preset is applied when a delivery is handled (Webhook.WithProviderDefaults),
// never when a definition is saved: the stored definition keeps what its
// author wrote, and a corrected preset reaches existing definitions.

// WebhookProvider is one forge preset.
type WebhookProvider struct {
	// AuthKind, SignatureHeader, EventHeader and DeliveryIDHeader fill the
	// definition's own auth.kind, auth.header, event_header and
	// auth.delivery_id_header when it leaves them empty.
	AuthKind         string
	SignatureHeader  string
	EventHeader      string
	DeliveryIDHeader string
	// PingEvent is the event the forge sends to test a hook ("" for none). A
	// definition listing events answers it with 200 and starts nothing.
	PingEvent string
	// Shapes are the paths the body of each event the preset knows must
	// hold, keyed by the event as a definition lists it.
	Shapes map[string][]string
	// canonical turns the event header's value into the spelling a
	// definition lists; nil leaves it as sent.
	canonical func(string) string
}

// A shape is a set of paths a body must hold, so it can tell an event apart
// only from events whose bodies lack one of them: a body of ANY other event
// — listed or not — that holds them all passes as this one. A preset
// therefore gives a shape only to an event no other event's body is known to
// hold. GitHub's issues is not among them (an issue_comment body holds every
// path an issues body does), nor create and delete (the same paths), nor, on
// Gitea, pull_request (its review events send the same body). Such an event
// can still be listed, with a shape of the author's own in event_shapes.
//
// Paths shared by GitHub and Gitea, whose payloads Gitea models on GitHub's.
var githubLikeShapes = map[string][]string{
	"push":          {"$.ref", "$.before", "$.after", "$.repository.id"},
	"issue_comment": {"$.action", "$.issue.number", "$.comment.id", "$.repository.id"},
	"release":       {"$.action", "$.release.id", "$.repository.id"},
}

var webhookProviders = map[string]WebhookProvider{
	"github": {
		AuthKind: "hmac", SignatureHeader: "X-Hub-Signature-256",
		EventHeader: "X-GitHub-Event", DeliveryIDHeader: "X-GitHub-Delivery",
		PingEvent: "ping",
		Shapes: mergeShapes(githubLikeShapes, map[string][]string{
			"pull_request":                {"$.action", "$.number", "$.pull_request.head.sha", "$.repository.id"},
			"pull_request_review":         {"$.action", "$.review.id", "$.pull_request.number", "$.repository.id"},
			"pull_request_review_comment": {"$.action", "$.comment.id", "$.pull_request.number", "$.repository.id"},
			"workflow_run":                {"$.action", "$.workflow_run.id", "$.repository.id"},
		}),
	},
	"gitea": {
		AuthKind: "hmac", SignatureHeader: "X-Gitea-Signature",
		EventHeader: "X-Gitea-Event", DeliveryIDHeader: "X-Gitea-Delivery",
		Shapes: githubLikeShapes,
	},
	"gitlab": {
		// A shared token sent in the clear inside TLS, not a signature: the
		// body is not signed, so these shapes check that a delivery is what
		// its header says, and protect nothing from whoever holds the token.
		AuthKind: "bearer", SignatureHeader: "X-Gitlab-Token",
		EventHeader: "X-Gitlab-Event", DeliveryIDHeader: "X-Gitlab-Event-UUID",
		Shapes: map[string][]string{
			"push":          {"$.object_kind", "$.ref", "$.before", "$.after", "$.project.id"},
			"tag_push":      {"$.object_kind", "$.ref", "$.before", "$.after", "$.project.id"},
			"merge_request": {"$.object_kind", "$.object_attributes.iid", "$.object_attributes.last_commit.id", "$.project.id"},
			"note":          {"$.object_kind", "$.object_attributes.id", "$.project.id"},
			"issue":         {"$.object_kind", "$.object_attributes.iid", "$.project.id"},
			"pipeline":      {"$.object_kind", "$.object_attributes.id", "$.project.id"},
		},
		// "Push Hook" → push, "Merge Request Hook" → merge_request.
		canonical: func(v string) string {
			return strings.ReplaceAll(strings.ToLower(strings.TrimSuffix(v, " Hook")), " ", "_")
		},
	},
}

func mergeShapes(a, b map[string][]string) map[string][]string {
	out := make(map[string][]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// WebhookProviderPreset returns the preset named name.
func WebhookProviderPreset(name string) (WebhookProvider, bool) {
	p, ok := webhookProviders[name]
	return p, ok
}

// WebhookProviderNames lists the presets, sorted.
func WebhookProviderNames() []string {
	out := make([]string, 0, len(webhookProviders))
	for n := range webhookProviders {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// WithProviderDefaults returns w with its provider preset's defaults in the
// fields w leaves empty. Without a provider, or with one this server does not
// know (refused when the definition is written), w is returned as it is.
func (w Webhook) WithProviderDefaults() Webhook {
	p, ok := webhookProviders[w.Provider]
	if !ok {
		return w
	}
	if w.Auth.Kind == "" {
		w.Auth.Kind = p.AuthKind
	}
	if w.Auth.Header == "" {
		w.Auth.Header = p.SignatureHeader
	}
	if w.Auth.DeliveryIDHeader == "" {
		w.Auth.DeliveryIDHeader = p.DeliveryIDHeader
	}
	if w.EventHeader == "" {
		w.EventHeader = p.EventHeader
	}
	return w
}

// CanonicalEvent is the event a header value names, as a definition lists
// it: the provider's spelling turned into the listed one.
func (w Webhook) CanonicalEvent(value string) string {
	if p, ok := webhookProviders[w.Provider]; ok && p.canonical != nil {
		return p.canonical(value)
	}
	return value
}

// IsPingEvent reports whether event is the provider's ping.
func (w Webhook) IsPingEvent(event string) bool {
	p, ok := webhookProviders[w.Provider]
	return ok && p.PingEvent != "" && event == p.PingEvent
}

// EventShape returns the paths the body of event must hold: the
// definition's own event_shapes entry, else its provider's.
func (w Webhook) EventShape(event string) ([]string, bool) {
	if s, ok := w.EventShapes[event]; ok {
		return s, true
	}
	if p, ok := webhookProviders[w.Provider]; ok {
		s, ok := p.Shapes[event]
		return s, ok
	}
	return nil, false
}

// ListsEvent reports whether event is one of w's events.
func (w Webhook) ListsEvent(event string) bool {
	for _, e := range w.Events {
		if e == event {
			return true
		}
	}
	return false
}

// webhookEventRe is what an event header's value, and an event a definition
// lists, may be: a short plain token. GitLab's carry spaces ("Push Hook").
var webhookEventRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_ .:-]{0,63}$`)

// ValidWebhookEvent reports whether v may be an event header's value.
func ValidWebhookEvent(v string) bool { return webhookEventRe.MatchString(v) }

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

const (
	maxWebhookEvents     = 64
	maxWebhookShapePaths = 32
)

// CheckWebhookEvents checks provider, event_header, events and event_shapes
// when a definition is written (the yaml load and the WebhookDef tool both
// call it). A definition that sets none of them is not affected.
//
// Every listed event must have a shape, from the preset or event_shapes: an
// event with none would count on its unsigned header alone, without anyone
// noticing that the protection was absent.
func CheckWebhookEvents(w Webhook) error {
	if w.Provider != "" {
		if _, ok := webhookProviders[w.Provider]; !ok {
			return fmt.Errorf("provider %q is not one this server knows (want one of: %s)", w.Provider, strings.Join(WebhookProviderNames(), ", "))
		}
	}
	if w.EventHeader != "" && !headerNameRe.MatchString(w.EventHeader) {
		return fmt.Errorf("event_header %q is not a header name", w.EventHeader)
	}
	if len(w.Events) == 0 {
		if len(w.EventShapes) > 0 {
			return fmt.Errorf("event_shapes is set without events: shapes are checked only for listed events")
		}
		return nil
	}
	eff := w.WithProviderDefaults()
	if eff.EventHeader == "" {
		return fmt.Errorf("events needs the header that names a delivery's event: set event_header, or a provider (%s)", strings.Join(WebhookProviderNames(), ", "))
	}
	if len(w.Events) > maxWebhookEvents {
		return fmt.Errorf("events lists %d events; at most %d", len(w.Events), maxWebhookEvents)
	}
	seen := map[string]bool{}
	for _, e := range w.Events {
		if !webhookEventRe.MatchString(e) {
			return fmt.Errorf("events: %q is not an event name (letters, digits, _ . : - and spaces, at most 64)", e)
		}
		if seen[e] {
			return fmt.Errorf("events lists %q twice", e)
		}
		seen[e] = true
		if w.IsPingEvent(e) {
			return fmt.Errorf("events lists %q: the receiver answers the provider's ping itself and starts nothing for it", e)
		}
		if _, ok := w.EventShape(e); !ok {
			return fmt.Errorf("events lists %q, which has no shape: the event header is not signed, so an event counts only when the signed body has its shape. "+
				"Give it one in event_shapes (the JSONPaths its body always holds)", e)
		}
	}
	names := make([]string, 0, len(w.EventShapes))
	for e := range w.EventShapes {
		names = append(names, e)
	}
	sort.Strings(names)
	for _, e := range names {
		if !seen[e] {
			return fmt.Errorf("event_shapes[%q] is not one of events", e)
		}
		paths := w.EventShapes[e]
		if len(paths) == 0 || len(paths) > maxWebhookShapePaths {
			return fmt.Errorf("event_shapes[%q] needs 1 to %d paths", e, maxWebhookShapePaths)
		}
		for _, p := range paths {
			segs, err := jsonpath.Parse(p)
			if err != nil {
				return fmt.Errorf("event_shapes[%q]: %q is not a supported JSONPath ($, .key and [N] only): %v", e, p, err)
			}
			if len(segs) == 0 {
				return fmt.Errorf("event_shapes[%q]: %q names the whole body, which every delivery has", e, p)
			}
		}
	}
	return nil
}
