-- 0113_token_usage_purpose.up.sql — what a usage row was for, and the audio
-- a speech-to-text call transcribed.
--
-- OCR and speech-to-text calls are charged on the same ledger as chat calls,
-- so the usage report, the per-run cost and the token budgets keep one source.
-- purpose tells them apart: 'chat' (every call so far, and the default), 'ocr'
-- (a vision call reading a page — an ordinary token row) or 'stt' (a
-- transcription call, priced per minute, with no tokens). audio_seconds is the
-- length of audio an STT call transcribed; 0 on every other row. Nothing is
-- backfilled: an old row reads back chat with no seconds, which is what it was.
--
-- The archive keeps the split. purpose joins its key, so a rolled-up day holds
-- one row per purpose instead of merging an STT row's cost into a chat row
-- with no record of the minutes; audio_seconds is summed like the tokens. Every
-- existing archive row is 'chat', so the wider key admits them all.
--
-- Non-secret: a label and a duration.
--
-- IF [NOT] EXISTS throughout: the memory_embeddings repair test rewinds the
-- version pointer and replays every later migration.
ALTER TABLE token_usage   ADD COLUMN IF NOT EXISTS purpose       TEXT             NOT NULL DEFAULT 'chat';
ALTER TABLE token_usage   ADD COLUMN IF NOT EXISTS audio_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE usage_archive ADD COLUMN IF NOT EXISTS purpose       TEXT             NOT NULL DEFAULT 'chat';
ALTER TABLE usage_archive ADD COLUMN IF NOT EXISTS audio_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE usage_archive DROP CONSTRAINT IF EXISTS usage_archive_pkey;
ALTER TABLE usage_archive ADD CONSTRAINT usage_archive_pkey
    PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source, purpose);
