-- 0116_token_usage_input_chars.up.sql — the characters a text-to-speech call
-- sent to its engine.
--
-- A text-to-speech call is charged on the same ledger as chat, OCR and
-- speech-to-text calls (purpose 'tts', a value of the TEXT column 0113 added,
-- so no constraint changes). Engines bill it by the characters sent (OpenAI
-- tts-1) or by the audio produced (per minute, already audio_seconds), so the
-- ledger keeps both units. input_chars is 0 on every other row; nothing is
-- backfilled, as no row before this carried text to speech.
--
-- The archive sums it per key like audio_seconds. The key does not change:
-- purpose is already in it, so a day's TTS rows fold into their own row.
--
-- Non-secret: a count.
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- and replays every later migration.
ALTER TABLE token_usage   ADD COLUMN IF NOT EXISTS input_chars BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_archive ADD COLUMN IF NOT EXISTS input_chars BIGINT NOT NULL DEFAULT 0;
