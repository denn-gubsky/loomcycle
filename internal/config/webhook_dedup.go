package config

import (
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// A webhook's dedup_key names one event by fields of its signed body — for a
// push, the repository, the ref and the new head — so a forge's redelivery is
// a duplicate however its delivery id or its bytes differ. The key is held in
// the store for dedup_ttl_seconds, on every replica, and records the run it
// started: a duplicate is answered as one unless that run ended failed, so a
// run lost in a restart does not keep the event from being handled again.

const (
	maxWebhookDedupPaths = 16
	// DefaultWebhookDedupTTL is how long a dedup_key is held when the
	// definition does not say.
	DefaultWebhookDedupTTL = 7 * 24 * time.Hour
	// MaxWebhookDedupTTL is the longest a definition may hold one.
	MaxWebhookDedupTTL = 90 * 24 * time.Hour
)

// CheckWebhookDedupKey checks a definition's dedup_key and its lifetime when
// it is written: 1 to 16 paths, each parsing and none repeated or the whole
// body, and a lifetime only beside a key, of at most 90 days.
func CheckWebhookDedupKey(w Webhook) error {
	if len(w.DedupKey) > maxWebhookDedupPaths {
		return fmt.Errorf("dedup_key holds %d paths; at most %d", len(w.DedupKey), maxWebhookDedupPaths)
	}
	seen := make(map[string]bool, len(w.DedupKey))
	for i, p := range w.DedupKey {
		segs, err := jsonpath.Parse(p)
		if err != nil {
			return fmt.Errorf("dedup_key[%d]: %q is not a supported JSONPath ($, .key and [N] only): %v", i, p, err)
		}
		if len(segs) == 0 {
			return fmt.Errorf("dedup_key[%d]: %q names the whole body; the body hash is already a delivery's identity", i, p)
		}
		if seen[p] {
			return fmt.Errorf("dedup_key[%d]: %q is listed twice", i, p)
		}
		seen[p] = true
	}
	switch {
	case w.DedupTTLSeconds < 0:
		return fmt.Errorf("dedup_ttl_seconds is %d; it cannot be negative", w.DedupTTLSeconds)
	case w.DedupTTLSeconds > 0 && len(w.DedupKey) == 0:
		return fmt.Errorf("dedup_ttl_seconds is set without dedup_key, so nothing would be held for it")
	case time.Duration(w.DedupTTLSeconds)*time.Second > MaxWebhookDedupTTL:
		return fmt.Errorf("dedup_ttl_seconds is %d; at most %d (90 days)", w.DedupTTLSeconds, int(MaxWebhookDedupTTL/time.Second))
	}
	return nil
}

// DedupTTL is how long the definition's dedup_key is held.
func (w Webhook) DedupTTL() time.Duration {
	if w.DedupTTLSeconds <= 0 {
		return DefaultWebhookDedupTTL
	}
	return time.Duration(w.DedupTTLSeconds) * time.Second
}
