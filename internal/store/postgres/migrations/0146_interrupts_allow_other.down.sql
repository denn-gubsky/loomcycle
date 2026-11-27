-- 0146_interrupts_allow_other.down.sql — drop RFC HG P1's allow_other column:
-- every question takes only one of its options again.
ALTER TABLE interrupts DROP COLUMN IF EXISTS allow_other;
