---
name: History/window
description: "History op=window — the conversation turns around the one a stored fact was distilled from, found by the fact's source span; not the whole chat."
---
`window` follows a remembered fact back to where it was said. A fact from
`Memory op=recall` is one condensed sentence and often drops the specific you
need — a date, a name, the reason. The fact also carries the verbatim text it
came from (`source`) and the chat it came from (`source_session_id`). Pass
both here and you get **that turn plus a few neighbours**, not the whole chat.
The one thing to get right: `quote` is the fact's `source` span, copied
exactly — not the fact sentence itself.

## Arguments

- `session_id` (required) — the chat; use the fact's `source_session_id`.
- `quote` (required) — the fact's `source` text, verbatim. It is matched
  exactly first, then ignoring case and whitespace differences; nothing looser.
- `context` — turns to return on each side of the match (default 2, at most
  10; larger values are cut to 10).
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

## Returns

Found: `{scope, session_id, matched: true, matched_turn, first_turn,
total_turns, turns: [{speaker, text, seq, at}], markdown}`. `matched_turn` and
`first_turn` are turn positions counted from 0; `at` is when the turn was said;
`markdown` is the same turns as `### user` / `### assistant` sections.

Not found: `{scope, session_id, matched: false, turns: [], total_turns, note}`
— the span is not in that chat (it was reworded before it was stored, or the
turn was redacted). This is a result, not an error, and no turns are guessed.

## Errors

- `history: window requires quote` — pass the fact's `source` span.
- `history: window: history: chat "<id>" not found — the fact's span survives
  on the fact itself; only the surrounding turns are gone` — the chat was
  removed by data retention or erased, OR it is in another scope. Retry once with the right
  `scope`; if it still fails, answer from the fact's `source` span alone.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Reach through from a recalled fact to the turns around it:

```json
{"op": "window", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "quote": "push the launch to the 14th"}
```

```json result
{"scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "matched": true,
 "matched_turn": 6, "first_turn": 4, "total_turns": 31,
 "turns": [{"speaker": "user", "text": "Maria is out the week of the 7th.", "seq": 88, "at": "2026-09-20T09:14:02Z"}],
 "markdown": "### user\n\nMaria is out the week of the 7th.\n\n..."}
```

The same, with only the matching turn and one neighbour on each side:

```json
{"op": "window", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "quote": "push the launch to the 14th", "context": 1}
```
