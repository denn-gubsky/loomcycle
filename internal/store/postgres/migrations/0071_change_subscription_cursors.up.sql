-- RFC CD Part C (push): the persisted per-subscription delivery cursor. The
-- outbound change-subscription engine advances last_seq after a successful
-- signed delivery, so push resumes at-least-once across a restart (the
-- subscriber dedupes on the monotonic seq). Keyed by the operator-declared
-- subscription name (change_subscriptions.<name>).
CREATE TABLE IF NOT EXISTS change_subscription_cursors (
	name       TEXT PRIMARY KEY,
	last_seq   BIGINT NOT NULL DEFAULT 0,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
