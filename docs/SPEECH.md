# Speech: read-aloud and transcription for clients

A chat client can read a reply aloud and take a spoken message, on the deployment's own speech engines, without starting an agent run. This guide covers the two routes that do it, how a client finds out whether to show the controls, and what an operator sets up.

## Why these routes, and not the raw model gateway

The LLM gateway (`/v1/chat/completions`, `/v1/embeddings`) trusts the caller with the provider and model it names, so it is gated by `llm:gateway`, which an isolated (self-signed-up) user does not hold. The speech routes give the caller no such choice: the engines, models and voices are the operator's configuration, and a caller sends only text or a recording and picks a language and a voice from the operator's list. So they are gated as a chat user's call, at `runs:create`, and an isolated user can use them.

They are also not run-plane routes: reading aloud is not part of starting or following a run, so a token limited to certain agents is refused, **unless it was granted the route's surface** when it was minted. That is how a chat bridge's per-person token transcribes that person's voice notes and voices replies to them, billed to them (see [Who may call them](#who-may-call-them)).

## The routes

| Route | In | Out |
|---|---|---|
| `POST /v1/audio/speech` | JSON `{input, model?, voice?, response_format?, language?}` | The audio, whole: `audio/wav`, `audio/mpeg` or `audio/ogg` |
| `POST /v1/audio/transcriptions` | multipart `{file, model?, language?, prompt?, response_format?}` | `{text, language, duration}`; `verbose_json` adds `segments`; `text` is `text/plain` |
| `GET /v1/_speech` | — | What a client needs to draw the controls |

The request shapes follow OpenAI's audio endpoints; fields an OpenAI client sends that the runtime does not use (`speed`, `temperature`, `timestamp_granularities[]`) are ignored, not refused.

### Read-aloud

`input` is Markdown or plain text. The runtime:

1. normalises the Markdown (no "asterisk", no URLs; a code block becomes a short spoken note);
2. picks the language: the caller's, else the text's script, else `speech.default_language`;
3. cuts the text into pieces of `speech.max_chars_per_call`;
4. asks the engine for WAV one piece at a time, with the language's voice (or one of its `alternates` the caller named);
5. joins the pieces, with a 200 ms pause at each paragraph end;
6. for `ogg` or `mp3`, encodes through the operator's `speech.encode`.

The audio comes back whole with a `Content-Length`; nothing is streamed, and a failure part-way returns no audio. The response headers say what was done: `X-Loomcycle-Speech-Language`, `-Voice`, `-Seconds`, `-Chars`, `-Pieces`. A browser can read them only on a same-origin request (the runtime exposes no CORS headers).

**Formats.** `wav` always works. There is one `speech.encode` command and it may fix its codec, so the only other format offered is `speech.format`, and only when `speech.encode` is set. Ask for anything else and the answer is a 400 `speech_encode_not_configured` naming `speech.encode`. `GET /v1/_speech` lists the formats this deployment returns.

### Transcription

The route runs the same pass as an agent's `File op=transcribe`, on the same engine:

- a **PCM WAV** of any length is cut into pieces the engine takes, and the overlap between pieces is removed;
- with `transcription.stt.media.command` (ffmpeg) set, any audio or video type is converted to WAV first;
- without it, a type the engine reads whole (webm, mp4/m4a, mp3, ogg, flac) is sent whole when it fits one engine request. A browser's `MediaRecorder` output (`audio/webm` in Chrome, `audio/mp4` in Safari) therefore works on a host with no ffmpeg, up to the engine's upload cap. Larger is a 413 and a type the engine does not read a 415, both naming `transcription.stt.media.command`.

A WAV's length is counted from its samples. Any other file is counted at the larger of its header's claim and its size at 8 kbit/s, then reconciled to the length the engine reports. A pass that stops part-way returns no transcript.

### What one request may carry

| | Bound |
|---|---|
| Read-aloud text | `speech.max_chars_per_run` characters and `speech.max_seconds_per_run` of estimated audio (413 `input_too_long`) |
| Recording length | `transcription.stt.max_minutes_per_run` (413 `audio_too_long`) |
| Request body | `LOOMCYCLE_MAX_REQUEST_BYTES`, default 16 MiB (413 `request_too_large`) |

The per-run caps are the per-request caps here: a request is the unit of work. An agent's own `speech:` / `transcription:` block does not apply.

### Under load: requests wait

Engine requests take turns in a server-wide queue per direction, sized by `LOOMCYCLE_SPEECH_MAX_INFLIGHT` and `LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT` (default 2 each). A request that finds the queue full **waits** for a slot while its caller stays connected; it is not refused with a 503. A slot is held for one engine request, so a long text or recording cut into pieces takes turns with shorter requests rather than holding the engine throughout. A caller who hangs up leaves the queue.

`/metrics` reports `loomcycle_audio_engine_inflight` and `loomcycle_audio_engine_queued`, per `direction`. A queue that keeps growing says the speech host needs more capacity, or the bound is set lower than it can serve.

A client should show "working…" rather than time out early, and give the user a cancel.

## Who may call them

- **Scope:** `runs:create`. A tenant operator and an admin hold it; so do members and isolated users.
- **Access control:** `use gateway:speech` and `use gateway:transcription`. The member and isolated baselines allow `use gateway:*`; a rule list can deny either one.
- **A token limited to certain agents** only with the surface granted: `"surfaces": ["speech", "transcription"]` beside its `agents` at mint (a per-user token, an operator token, a declared principal, or an external gate's `issued_token.surfaces` ceiling; CONFIGURATION.md, "Granting the speech routes"). Without the grant it gets 403 `restricted_token`. The grant opens only these routes and `GET /v1/_speech`; every other check here still applies.
- **Budgets:** a caller already over a hard token budget is refused (429 `token_limit_exceeded`) before the engine is asked. Each call also takes one start from the caller's run rate.
- **Keys:** a tenant's own provider key, and the operator-key restriction, apply as they do for a run (403 `operator_key_restricted`).

## Whether to show the controls

`GET /v1/_me` reports `effective.surfaces.speech` and `effective.surfaces.transcription`. Each is true only when its engine is configured **and** the caller would get past its route's gates (scope, run plane, access control), so a client never has to probe with a request that costs a ledger row.

`GET /v1/_speech` then says what to draw. A direction whose engine is off, or whose route this caller may not use, is `{"enabled": false}` and nothing else. It names no engine URL, provider or model; voice ids are listed because a caller has to pick one.

```json
{
  "speech": {
    "enabled": true, "default_language": "uk",
    "languages": {
      "uk": {"voices": ["ukrainian_tts"], "default_voice": "ukrainian_tts"},
      "en": {"voices": ["af_heart", "am_adam"], "default_voice": "af_heart"}
    },
    "formats": ["wav"],
    "max_chars_per_call": 4000, "max_chars_per_request": 100000, "max_seconds_per_request": 3600
  },
  "transcription": {
    "enabled": true, "default_language": "uk", "languages": ["uk", "ru", "en"],
    "accepts": ["audio/wav", "audio/webm", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/flac", "video/mp4", "video/webm"],
    "max_upload_bytes": 16777216, "max_seconds_per_call": 7200, "max_seconds_per_request": 18000,
    "whole_file_max_bytes": 26214400
  }
}
```

`transcription.languages` is present when the operator set `transcription.stt.languages`: a request (or an agent's `File op=transcribe`) naming any other language is a 400 `unsupported_language`, and one naming none is transcribed in `default_language`. Without the list, any supported code is taken.

With a converter configured, `accepts` is `["audio/wav", "audio/*", "video/*"]` and `whole_file_max_bytes` is absent. Without one, it lists `audio/wav` and the types the engine reads whole: OpenAI's formats, or the operator's `transcription.stt.accepts` (`[audio/wav]` for an engine on Ollama, which reads nothing else).

## From the TS client

```ts
const me = await client.whoami();
if (me.effective?.surfaces.speech) {
  const info = await client.speechInfo();
  const format = info.speech.formats?.includes("ogg") ? "ogg" : "wav";
  const audio = await client.speech(reply, { format, signal });
  play(new Blob([audio.bytes], { type: audio.contentType }));
}
if (me.effective?.surfaces.transcription) {
  const { text } = await client.transcribe(recording, { language: "uk", signal });
}
```

`transcribe` sends `FormData` and sets no `Content-Type`, so the runtime writes the multipart boundary. The Python adapter is gRPC-only and has no speech methods; the routes have no gRPC or MCP twin yet.

## Through a chat bridge

The bridges (`@loomcycle/bridge-*` 0.4.0) use these routes to exchange voice messages with an agent (RFC GA). With `BRIDGE_VOICE=1`, a bridge:

1. downloads a voice message with the platform's credential, from the platform's own file host only;
2. transcribes it with the **conversation's own token**;
3. gives the agent the text after a `(voice message)` line, the way typed text goes;
4. when the reply should be spoken, voices it and posts it in the platform's own form, labelled "AI voice".

The text reply is always sent as well.

| Platform | Voice in | Voice out |
|---|---|---|
| Telegram | voice and round video notes (in a group, a reply to the bot) | a native voice note (`ogg` or `mp3`) |
| Discord | "Ask by voice" on your own voice message | a native voice message (`ogg`) |
| Slack | a clip or an audio file in a DM, or mentioning the bot | an audio file in the thread |
| Teams | an audio file attached in a 1:1 chat | text only |

**What loomcycle needs:**
- **Engines:** the `speech:` and `transcription.stt:` engines.
- **A voice format:** Telegram and Discord show only Opus as a voice note, so they need `speech.encode` (an ffmpeg Opus command) with `speech.format: ogg`. Slack plays `mp3` most reliably. With `wav` only, Telegram and Discord replies stay text, and Slack uploads `wav`.
- **A grant on each user token:** a bridge's user token is limited to its agent, so it reaches these routes only when granted them (`surfaces`, [Who may call them](#who-may-call-them)).
  - A bridge with a loomcycle token asks for the grant itself.
  - A bridge that is a gate is held to its `issued_token.surfaces` ceiling. Add `surfaces: [speech, transcription]` there, or the bridge stays on text.
- **Rate:** each voice turn takes three starts from the user's run rate (transcribe, run, speak), so raise `BRIDGE_RATE_PER_MINUTE`.

Each bridge's README has its platform's details and the settings (`BRIDGE_VOICE_*`, `/voice on|off|auto`). `@loomcycle/bridge-core`'s README has the shared ones.

## What it costs

Each engine request writes one usage row filed under the caller and its tenant, with no run: `purpose: tts` with `input_chars` and `audio_seconds`, or `purpose: stt` with `audio_seconds`. They are priced from the `pricing:` table (`per_million_chars`, `per_minute`, or tokens for a token-billed model) and show in `GET /v1/_usage?group_by=purpose`. The token budgets count tokens only: a row adds the tokens a token-billed model reported (e.g. `gpt-4o-transcribe`), and none for an engine billed by the minute or by the character.

## Refusals

| Status | Code | Means |
|---|---|---|
| 400 | `speech_encode_not_configured` / `unsupported_format` | A format this deployment does not return. |
| 400 | `unsupported_voice` / `unsupported_language` / `empty_input` | Fix the request; the message names what is allowed. |
| 400 | `model_not_allowed` | Read-aloud: not a `synthesizer` alias. Transcription: omit `model`. |
| 400 | `unreadable_audio` / `unsupported_response_format` / `bad_prompt` / `bad_request` | The upload or a field is wrong. |
| 403 | `insufficient_scope` / `acl_denied` / `restricted_token` / `operator_key_restricted` | The caller may not use it. |
| 413 | `input_too_long` / `audio_too_long` / `audio_too_large` / `request_too_large` | Send it in parts, or as a PCM WAV. |
| 415 | `unsupported_media_type` | Not a recording, or a type the engine does not read and there is no converter. |
| 429 | `token_limit_exceeded` | The caller is at a hard token budget. |
| 502 | `speech_failed` / `transcription_failed` | The engine failed; the detail is in the server log, never in the reply. |
| 503 | `speech_not_configured` / `transcription_not_configured` / `transcription_unavailable` | No such engine here, or it cannot be reached. |

## Setting it up

The engines are configured in `speech:` and `transcription.stt:`; see `docs/CONFIGURATION.md` §9h and §9i. A self-hosted [speaches](https://github.com/speaches-ai/speaches) server serves both the OpenAI speech and transcription endpoints; install a model with `POST /v1/models/{id}` on it. In a container, its model cache must be writable by the user it runs as (uid 1000 in the published image). Nothing else is needed for WAV in and WAV out; ffmpeg (the `loomcycle-media` image carries it) adds encoded read-aloud and converts any recording.
