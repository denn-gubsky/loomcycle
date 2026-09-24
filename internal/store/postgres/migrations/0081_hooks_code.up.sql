-- RFC DK-P2: a hook's body may be code-js instead of a webhook.
--
-- A hook has exactly one body: callback_url (a webhook) or code (JavaScript
-- run in-process in the code-js sandbox). A code hook stores '' in
-- callback_url, which stays NOT NULL so existing readers are unchanged, and
-- existing rows backfill to '' here — every hook registered before this
-- migration is a webhook.
ALTER TABLE hooks ADD COLUMN IF NOT EXISTS code TEXT NOT NULL DEFAULT '';
