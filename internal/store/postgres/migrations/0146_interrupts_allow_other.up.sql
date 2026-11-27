-- 0146_interrupts_allow_other.up.sql — RFC HG P1: an answer outside the
-- options.
--
-- allow_other is the agent's choice per question: with options, the resolve
-- paths also accept an answer of the person's own (non-empty, capped). False
-- for every row before, so an existing question still takes only one of its
-- options. Mirrors the SQLite column pass.
ALTER TABLE interrupts ADD COLUMN IF NOT EXISTS allow_other BOOLEAN NOT NULL DEFAULT FALSE;
