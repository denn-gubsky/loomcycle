// Package changesub delivers value-free memory/document change events to
// operator-declared HTTP callbacks (RFC CD Part C — push). It is the outbound
// twin of the SSE change feed: an engine tails the change feed per subscription,
// filters by (scope, kinds), and POSTs HMAC-signed batches to a callback.
//
// Delivery is AT-LEAST-ONCE: a persisted per-subscription cursor (advanced only
// after a successful POST) resumes across restarts, and the subscriber dedupes
// on the monotonic seq. The signature is X-Loomcycle-Signature =
// hex(hmac-sha256(secret, body)) — the same scheme loomcycle's inbound webhook
// verifier checks, so a peer can verify it and any consumer can with the
// documented scheme.
//
// The engine only runs when LOOMCYCLE_MEMORY_CHANGES_ENABLED (the feed exists)
// and at least one subscription is declared. The SSRF-guarded HTTP client and
// the secret-env allowlist are injected by the caller (main.go), so this
// package stays testable against an httptest receiver.
package changesub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Subscription is a resolved change subscription (config.ChangeSubscription +
// its name + the SSRF-guarded client the caller built for its host).
type Subscription struct {
	Name        string
	CallbackURL string
	TenantID    string
	Scope       string   // "" = any scope
	Kinds       []string // "" = all; exact types or the "memory"/"document" family
	SecretEnv   string   // "" = unsigned
	Client      *http.Client
}

// Store is the subset of store.Store the deliverer needs.
type Store interface {
	GetMemoryChangesSince(ctx context.Context, tenantID string, afterSeq int64, limit int) ([]store.MemoryChange, error)
	GetChangeSubscriptionCursor(ctx context.Context, name string) (int64, error)
	SetChangeSubscriptionCursor(ctx context.Context, name string, seq int64) error
}

// Deliverer delivers pending change events to subscriptions.
type Deliverer struct {
	store      Store
	secretFor  func(envName string) (string, error) // allowlist-gated env resolve
	logf       func(string, ...any)
	batchLimit int
	maxRetries int
}

// New builds a Deliverer. secretFor resolves an env-var NAME to its value
// (allowlist-gated); logf may be nil.
func New(s Store, secretFor func(string) (string, error), logf func(string, ...any)) *Deliverer {
	return &Deliverer{store: s, secretFor: secretFor, logf: logf, batchLimit: 200, maxRetries: 3}
}

// RunOnce delivers pending changes for every subscription, draining each
// subscription's backlog. Called on a ticker (advisory-gated in cluster mode).
func (d *Deliverer) RunOnce(ctx context.Context, subs []Subscription) {
	for _, sub := range subs {
		for {
			fetched, err := d.deliverOnce(ctx, sub)
			if err != nil {
				if d.logf != nil {
					d.logf("change_subscriptions.%s: delivery failed (will retry): %v", sub.Name, err)
				}
				break
			}
			if fetched < d.batchLimit || ctx.Err() != nil {
				break // drained (or cancelled)
			}
		}
	}
}

// deliverOnce processes one batch for a subscription. It fetches the next window
// of changes past the cursor, POSTs the ones matching the filter, and — only on
// a successful POST — advances + persists the cursor over the WHOLE fetched
// window (filtered-out rows are skipped, not re-scanned). Returns how many rows
// were fetched (== batchLimit means more may remain). On a POST failure the
// cursor is left untouched so the next tick re-attempts (at-least-once).
func (d *Deliverer) deliverOnce(ctx context.Context, sub Subscription) (int, error) {
	cursor, err := d.store.GetChangeSubscriptionCursor(ctx, sub.Name)
	if err != nil {
		return 0, fmt.Errorf("read cursor: %w", err)
	}
	changes, err := d.store.GetMemoryChangesSince(ctx, sub.TenantID, cursor, d.batchLimit)
	if err != nil {
		return 0, fmt.Errorf("read changes: %w", err)
	}
	if len(changes) == 0 {
		return 0, nil
	}
	maxSeq := changes[len(changes)-1].Seq

	matched := make([]store.MemoryChange, 0, len(changes))
	for _, ch := range changes {
		if matches(ch, sub) {
			matched = append(matched, ch)
		}
	}
	if len(matched) > 0 {
		body, mErr := json.Marshal(deliveryBatch{Subscription: sub.Name, Changes: matched})
		if mErr != nil {
			return 0, mErr
		}
		if pErr := d.post(ctx, sub, body); pErr != nil {
			return 0, pErr // leave the cursor — retry next tick
		}
	}
	if err := d.store.SetChangeSubscriptionCursor(ctx, sub.Name, maxSeq); err != nil {
		return 0, fmt.Errorf("advance cursor: %w", err)
	}
	return len(changes), nil
}

// deliveryBatch is the POST body — value-free (each MemoryChange is a coordinate,
// not a value).
type deliveryBatch struct {
	Subscription string               `json:"subscription"`
	Changes      []store.MemoryChange `json:"changes"`
}

func matches(ch store.MemoryChange, sub Subscription) bool {
	if sub.Scope != "" && string(ch.Scope) != sub.Scope {
		return false
	}
	if len(sub.Kinds) == 0 {
		return true
	}
	t := string(ch.Type)
	for _, k := range sub.Kinds {
		switch {
		case k == t:
			return true
		case k == "memory" && strings.HasPrefix(t, "memory."):
			return true
		case k == "document" && strings.HasPrefix(t, "document."):
			return true
		}
	}
	return false
}

// post signs + POSTs the body, retrying with bounded backoff on a transport
// error or non-2xx. A secret-resolution failure (config error) is NOT retried.
// The body carries no secret; errors carry only the env-var name.
func (d *Deliverer) post(ctx context.Context, sub Subscription, body []byte) error {
	var sig string
	if sub.SecretEnv != "" {
		secret, err := d.secretFor(sub.SecretEnv)
		if err != nil {
			return fmt.Errorf("signing key: %w", err) // config error — don't retry
		}
		sig = sign(secret, body)
	}
	var lastErr error
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.CallbackURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "loomcycle-change-subscription")
		req.Header.Set("X-Loomcycle-Subscription", sub.Name)
		if sig != "" {
			req.Header.Set("X-Loomcycle-Signature", sig)
		}
		resp, err := sub.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
		}
		lastErr = fmt.Errorf("callback %s returned %d", sub.CallbackURL, resp.StatusCode)
	}
	return lastErr
}

// sign returns hex(hmac-sha256(secret, body)) — symmetric with loomcycle's
// inbound webhook verifier (internal/api/webhook/signature.go compareHMAC).
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func backoff(attempt int) time.Duration {
	d := 100 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 4
	}
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}
