-- 0011_interrupts.up.sql — v0.8.16 Interruption tool.
--
-- Agents call Interruption.ask / .notify / .cancel; pending rows
-- block the run until resolved by a human (via Web UI), an external
-- MCP-server delivery surface, or a timeout / cancel. The kind column
-- is the closed-enum future-proofing for v0.9.x pause / wait_until /
-- approval — v0.8.16 writes only kind='question'.
--
-- user_id / agent_id / agent_name are denormalised from the run row
-- at create time (NOT joined on read) so the GET /v1/users/{id}/
-- interrupts listing query never needs a JOIN against runs. Same
-- denormalisation pattern as runs.user_id and channel_messages.run_id.
--
-- NO foreign key on run_id: interrupts must survive any future run
-- pruning, same reasoning as evaluations. Referential integrity is
-- enforced at the application layer (InterruptCreate validates the
-- run exists before inserting). RESTRICT FK would block legitimate
-- admin pruning; CASCADE would silently delete audit data.
--
-- answer_meta is the kind-discriminated structured-response slot:
-- v0.8.16 question writes either NULL or empty JSON; future approval
-- writes {approved: bool, reason?: string}. The agent-visible result
-- is the scalar `answer` field — `answer_meta` is for the resolver's
-- structured payload that doesn't fit a single string.

CREATE TABLE interrupts (
    interrupt_id    TEXT             PRIMARY KEY,
    run_id          TEXT             NOT NULL,
    kind            TEXT             NOT NULL DEFAULT 'question',
    status          TEXT             NOT NULL DEFAULT 'pending',
    question        TEXT,
    options         JSONB,
    context_data    TEXT,
    priority        TEXT             NOT NULL DEFAULT 'normal',
    answer          TEXT,
    answer_meta     JSONB,
    created_at      TIMESTAMPTZ      NOT NULL,
    expires_at      TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,
    resolved_by     TEXT,
    user_id         TEXT,
    agent_id        TEXT,
    agent_name      TEXT
);

-- Primary access patterns:
--   1. "Is this run blocked on a pending interrupt?"  → (run_id, status)
--   2. "What does this user need to answer?"          → (user_id, status)
--   3. "Sweeper: what timeouts have fired?"           → (expires_at, status)

CREATE INDEX interrupts_by_run_status  ON interrupts(run_id, status);
CREATE INDEX interrupts_by_user_status ON interrupts(user_id, status) WHERE user_id IS NOT NULL;
CREATE INDEX interrupts_by_expires     ON interrupts(expires_at)
    WHERE expires_at IS NOT NULL AND status = 'pending';
