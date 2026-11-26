# @loomcycle/bridge-teams

A Microsoft Teams bot for a [loomcycle](https://github.com/denn-gubsky/loomcycle) interactive agent. People chat with the agent in Teams. In a 1:1 chat each person is their own loomcycle user, with their own runs, memory, history and rate limit, and a token budget of their own when loomcycle holds one for them (the gate's `new_user.budget`, below); a group chat or channel is a loomcycle user of its own.

- **Streamed answers** in 1:1 chats, with Teams' Stop button. Group chats and channels get one message per turn.
- **Steering:** a message sent while the agent is working reaches it mid-turn.
- **Questions and reviews** from the agent arrive as cards with buttons.
- **Commands:** `/new` (start over), `/stop` (stop the current answer), `/help`.
- **Documents** (since 0.6, with `BRIDGE_FILES=1`): a file attached in a 1:1 chat is put into the agent's run inbox for it to read. See [Files and recordings](#files-and-recordings).
- **Recordings** (since 0.4, with `BRIDGE_VOICE=1`): an audio file attached in a 1:1 chat is transcribed and answered in text. See [Recordings](#recordings).
- **Restarts lose nothing:** the next message continues the conversation, with the agent's full history.

Built on [`@loomcycle/bridge-core`](../bridge-core), the core every loomcycle bridge shares (Slack, Telegram, Discord). Design and decisions: RFC EN (`/loomcycle/rfcs/rfc-en-teams-bridge`) and RFC EQ (`/loomcycle/rfcs/bridges`) in the loomcycle document store.

## Why a bridge, and not Copilot Studio or A2A

loomcycle's A2A server treats each message as a new run, so a Copilot Studio agent calling it loses the conversation, steering and streaming, and Copilot's own model writes the reply. Calling loomcycle's MCP server from Copilot makes loomcycle a tool rather than the agent. This bridge keeps the loomcycle agent in charge of the conversation and translates only the Teams side.

## How it works

```
Teams ──▶ Bot Connector ──HTTPS /api/messages──▶ teams-bridge ──private network──▶ loomcycle
```

- **Only the bridge is public.** loomcycle stays on your private network.
- **One interactive run per Teams conversation.** The bridge acknowledges each message at once and renders the reply in the background, so a long agent turn never times out a Teams request.
- **Per-user tokens.** loomcycle makes a token's subject authoritative over a request's `user_id`, so one service token cannot act for many users. The bridge holds a tenant-operator token and uses it for two things only: registering each Teams user (`teams-<entra object id>`) and minting that user a short-lived token. A user's token can start only the configured agent, may set no per-run override except `interactive`, and is rate-limited. Tokens live in memory only. As a loomcycle gate (below) the bridge holds no token at all.

## Setup

### 1. loomcycle

Run loomcycle with authentication on and a persistent store. Then:

1. Pick or define the agent the bot runs, for example the `chat` agent from `LOOMCYCLE_PRESETS=base,chat`.
2. Mint the bridge's token, a tenant operator in the tenant the Teams users will live in:

   ```bash
   loomcycle operator-token create --name teams-bridge --tenant acme --subject teams-bridge --scopes substrate:tenant
   ```

   Or declare it in YAML as a principal (`principals:` with `scopes: [substrate:tenant]` and a `token_env`).

### 2. Microsoft Entra and Azure Bot

1. Register a **single-tenant** app in Microsoft Entra ID and create a client secret. With files or voice on, also upload a certificate (or use a managed identity): from 2027-03-01 a client secret cannot download files ([The file token](#the-file-token-from-2027-03-01)).
2. Create an **Azure Bot** resource using that app. Set its messaging endpoint to `https://<bridge-host>/api/messages` and enable the **Microsoft Teams** channel.

### 3. Run the bridge

```bash
npx @loomcycle/bridge-teams
```

The container image is built from this repository's lockfile, which moves the Teams SDK's `@azure/msal-node` to a patched 7.1 release. An `npx` (or `npm install`) of the published package resolves the SDK's own range instead, which still allows msal-node 5.x until the SDK widens it. The advisory concerned is in a sign-in flow the bridge never calls, but prefer the image if you want the patched version.

Or run the container image, `denngubsky/loomcycle-bridge-teams` (also `ghcr.io/denn-gubsky/loomcycle-bridge-teams`), tagged with the package version. It takes the same environment variables. Mount a volume at `/data` and set `BRIDGE_STORE=file:/data/teams.json` to keep each conversation's session across restarts:

```bash
docker run --env-file teams-bridge.env -p 3978:3978 -v teams-bridge-data:/data denngubsky/loomcycle-bridge-teams:<version>
```

Configuration is by environment:

| Variable | Meaning | Default |
|---|---|---|
| `CLIENT_ID`, `CLIENT_SECRET`, `TENANT_ID` | The bot's Entra app | required |
| `PORT` | Listen port | `3978` |
| `LOOMCYCLE_BASE_URL` | loomcycle's URL. From `@loomcycle/bridge-core` 0.4.0: `https://`, or `http://` to localhost; over a private network set `BRIDGE_ALLOW_INSECURE_LOOMCYCLE=1` | required |
| `LOOMCYCLE_TOKEN` | The bridge's `substrate:tenant` token | required, unless the bridge is a gate (below) |
| `BRIDGE_AGENT` | The loomcycle agent every conversation runs | required |
| `TEAMS_BRIDGE_ALLOWED_TENANTS` | Comma list of Entra tenant ids allowed to use the bot | required |
| `BRIDGE_TOKEN_TTL_SECONDS` | Lifetime of a user's token | `43200` |
| `BRIDGE_RATE_PER_MINUTE`, `BRIDGE_RATE_BURST` | Runs a user may start per minute, and the burst | `6`, `10` |
| `BRIDGE_USER_ACCESS_MODE` | `isolated` (a user sees only their own data) or `tenant` | `isolated` |
| `BRIDGE_IDLE_MINUTES` | End a conversation's run after this long without a message; the next message continues it | `1440` |
| `BRIDGE_STORE` | `memory`, or `file:<path>` to survive restarts on one instance | `memory` |
| `BRIDGE_DEV` | `1` accepts unauthenticated requests, for local testing only: the listener then binds 127.0.0.1 only. From `@loomcycle/bridge-core` 0.4.0 it is refused on a gate | unset |
| `BRIDGE_FILES` | `1` puts documents attached in a 1:1 chat into the agent's run inbox ([Documents](#documents)); `BRIDGE_FILES_*` there | unset (off) |
| `BRIDGE_VOICE` | `1` transcribes audio files attached in a 1:1 chat ([Recordings](#recordings)) | unset (off) |
| `TEAMS_BRIDGE_FILE_HOSTS` | Comma list of host suffixes an attached file may be downloaded from | `.sharepoint.com` |
| `TEAMS_BRIDGE_DUMP_ACTIVITIES` | `1` logs every inbound activity, redacted ([What Teams sent](#what-teams-sent)) | unset (off) |
| `TEAMS_BRIDGE_FILES_GRAPH`, `TEAMS_BRIDGE_APP_ID`, `TEAMS_BRIDGE_NO_FILE_HINT` | Files Teams sends no link for, read through Graph, and the reply when none came ([the Graph fallback](#when-teams-sends-no-file-the-graph-fallback)) | `off` |

With `BRIDGE_STORE=memory`, a restart forgets which loomcycle session each Teams conversation was in, and the next message starts a fresh one. Use `file:<path>` (one instance) or your own `ConversationStore` (several).

### 4. The Teams app

1. In `appPackage/manifest.json`, replace the `${{...}}` placeholders. `BOT_ID` is the Entra app's client id.
2. Replace `color.png` (192×192) and `outline.png` (32×32, white on transparent) with your own icons if you like.
3. Zip the three files (no folder) and upload the zip in Teams (**Apps → Manage your apps → Upload an app**) or through the Teams admin center.

## As a loomcycle gate

Since 0.3 the bridge can be a loomcycle gate instead of holding a token: it signs who is talking with its own key, and loomcycle provisions the user and issues the token. A stolen bridge can then only speak for Teams users, never act as a tenant operator. Set `BRIDGE_GATE` and the bridge runs this way; without it, it runs with `LOOMCYCLE_TOKEN` as before.

1. Make a key pair. The private half goes to the bridge only:

   ```bash
   node -e 'const {privateKey,publicKey}=require("node:crypto").generateKeyPairSync("ed25519");
   console.log("BRIDGE_GATE_KEY=" + JSON.stringify(privateKey.export({format:"jwk"})));
   console.log("public_key: " + JSON.stringify(publicKey.export({format:"jwk"})))'
   ```

2. Register the gate in loomcycle, in the tenant the bridge used, **before** you switch the bridge:

   ```yaml
   auth:
     gates:
       teams:
         kind: external
         public_key: { kty: OKP, crv: Ed25519, x: "<the public x>" }
         issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], rate: { per_minute: 6, burst: 10 } }
         identity_map:
           tenant: acme
           provision: auto
           legacy_subject_prefix: teams-
           new_user: { budget: { soft_limit: 500000, hard_limit: 1000000 } }   # tokens a month, per user
   ```

   `issued_token` is required, or the gate does not load (write `issued_token: unrestricted` for tokens with the user's whole member reach). `new_user.budget` becomes each new user's token limit, so people the bot admits are capped without a limit written for each; leave it out and they have none.

   `legacy_subject_prefix: teams-` keeps the people the bridge already served: each person's first message through the gate links the user the bridge made (`teams-<entra object id>`, a group chat's or channel's `teams-conv-<hash>`), so their history, memory and documents stay theirs. Without it they would start again as new users `teams:<entra object id>`. New people are `teams:<entra object id>` either way.

   If people also sign in to the Web UI through an Entra OIDC gate keyed on the object id (`identity_map.subject: oid`), add `trusted: true` and `on_revoke: unlink` to the gate and `same_subject_as: <that gate's name>` to its `identity_map`: a person's first message then links to the user that gate made (`entra:<object id>`), so Teams and the Web UI share one user. With `provision: known` only people who have signed in through Entra get in. See "One user across two gates" in loomcycle's `docs/CONFIGURATION.md`.

3. Reconfigure the bridge: set `BRIDGE_GATE=teams`, `BRIDGE_GATE_AUDIENCE` (loomcycle's `LOOMCYCLE_PUBLIC_URL`) and `BRIDGE_GATE_KEY`, and remove `LOOMCYCLE_TOKEN` and `BRIDGE_USER_ACCESS_MODE`, which a gate bridge refuses. Then retire the old operator token. The other `BRIDGE_GATE_*` settings are in `@loomcycle/bridge-core`'s README.

   With `BRIDGE_STORE=file:<path>`, keep the store file: from `@loomcycle/bridge-core` 0.3.1 a conversation stored under its token-mode subject is carried over to the gate's subject the next time it is used, keeping its session and its owner.

As a gate the bridge also tells loomcycle about groups and leavers. Every person, group chat and channel is in the loomcycle group `teams:teams-tenant-<entra tenant id>` of its organisation, which an operator can bind permissions to; it takes `trusted: true` on the gate and an `identity_map.groups` block. (A channel gets no team group: the bridge keeps no team id.) A person who uninstalls the app from their 1:1 chat is revoked in loomcycle: disabled at once, then removed by the gate's removal policy — unless the gate says `on_revoke: unlink` and they also sign in through another gate (Entra), when only the Teams link goes and they stay active. Until loomcycle has taken the revoke the bridge answers Teams with an error, so the Bot Connector delivers the uninstall again. From `@loomcycle/bridge-core` 0.4.0 a revoke loomcycle does not take is also kept and retried by the bridge itself, so a loomcycle outage longer than the platform's redelivery no longer loses it. Teams lists no members the bridge can read, so there is no reconciliation: a person removed without an event reaching the bridge keeps access until an operator disables them in loomcycle.

## Group chats and channels

- **A shared chat runs as itself, never as one of its members.** A group chat, or a channel, is its own loomcycle user (`teams-conv-<hash>`), with its own memory and history. Anyone in it may talk to the agent, so no single person's private data or tools are reachable from it. A channel's threads share the channel's identity; each thread is its own run.
- Every message reaches the agent labelled with the speaker's name, so it knows who is talking.
- The person who started the conversation answers the agent's question and review cards, and only they can `/new` or `/stop`.
- Teams sends the bot channel messages only when it is @mentioned.

## Files and recordings

A person can attach two kinds of file in their 1:1 chat with the bot, each turned on separately: documents for the agent to read (`BRIDGE_FILES=1`), and recordings to transcribe (`BRIDGE_VOICE=1`). Both are off by default; with both off an attachment is ignored, as before.

### Documents

With `BRIDGE_FILES=1`, a person attaches a document (a passport `.docx`, a `.pdf`) and writes what they want, such as "check this". The bridge downloads the file, checks it, and puts it into the agent's run inbox with the person's own loomcycle token; the agent reads it with its File tool. Needs loomcycle 1.168 or later and `@loomcycle/client` 1.168 or later (the release that ships `putInboxFile`).

| Variable | Meaning | Default |
|---|---|---|
| `BRIDGE_FILES` | `1` takes attached documents | unset (off) |
| `BRIDGE_FILES_MAX_BYTES` | Largest file taken | `20971520` (20 MiB) |
| `BRIDGE_FILES_TYPES` | Types taken, by extension (the full list the bridge can check is in `@loomcycle/bridge-core`'s README) | `docx,pdf,odt,doc,xlsx,md,txt` |
| `BRIDGE_FILES_MAX_COUNT` | Most files taken from one message | `5` |
| `TEAMS_BRIDGE_FILE_HOSTS` | Where a file's download link may point (below) | `.sharepoint.com` |

**What happens.**

- **1:1 chats only.** A file in a group chat or a channel gets "I can only take files in a 1:1 chat with me." (Teams usually does not deliver files to a bot there at all.)
- **Checked twice.** The type Teams declares and the file name must agree and be in `BRIDGE_FILES_TYPES`, and the first bytes must match it: an `.exe` renamed `.docx` is refused.
- **Refused in plain words,** one message per reason: too large, not a type taken, could not download, not available here (the token lacks `inbox_put`, or the client is too old). Five files at most per message; the rest get one message saying how many were skipped.
- **What the agent sees:** the person's text, then one untrusted block of kind `attached_file` per file, holding what loomcycle answered the put with:

  ```
  {"name":"Pasport.docx","ref":"volume:inbox:documents/Pasport.docx","media_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","bytes":48213}
  ```

  Never the bytes, never the download link. (A loomcycle that takes no input blocks gets the same as text lines under a fixed "not verified data" header; see `@loomcycle/bridge-core`'s README.)
- **Two turns when no run is live.** A run's inbox exists only once the run does. After an idle end, `/new` or a restart, the bridge first starts the run with the person's text and a line saying files follow, then puts the files and sends their `attached_file` blocks as the next message. The agent may begin to answer before the files arrive.
- **A file lives as long as the run.** After the idle end (`BRIDGE_IDLE_MINUTES`) or `/new` the conversation continues in a new run, which does not see earlier files: the person sends the file again. (loomcycle's session inbox will lift this.)
- **Logs** say the reason, the declared type, sizes and statuses, never the file's name or link.

**What loomcycle needs.** Each user token needs the `inbox_put` surface. A bridge with a loomcycle token asks for it when it mints. For a gate bridge, add it to the gate's `issued_token`:

```yaml
issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], surfaces: [inbox_put] }   # add transcription too with voice on
```

Without it loomcycle refuses the put, and the person hears "I can't take files here." loomcycle's own caps apply as well: `LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES` per file and the run's inbox quota ("There's no room for more files in this conversation.").

With voice off, an audio file is a document like any other: taken when its type is in `BRIDGE_FILES_TYPES` (decision D4 of RFC HE), not transcribed. With voice on it is a recording (below).

### Recordings

With `BRIDGE_VOICE=1`, a person can send the bot a **recording as an audio file in their 1:1 chat**. The bridge transcribes it through loomcycle with the person's own token and gives the agent the text, marked `(voice message)`. The agent answers in text. The rest of the voice machinery (length and size caps, `BRIDGE_VOICE_*` settings, what the agent sees) is `@loomcycle/bridge-core`'s; see its README.

**What Teams allows a bot, and so what this bridge takes:**

- **Audio files in a 1:1 chat only.** The app package sets `supportsFiles: true`, which lets a person attach a file in a personal chat with the bot. Teams drops file attachments to bots in group chats and channels, so there a file is ignored, as before.
- **No voice notes.** The Teams voice-note button is not offered in a chat with a bot. In a channel, a voice note reaches a bot without its audio, which only Microsoft Graph's tenant-wide `ChannelMessage.Read.All` permission can read. The bridge does not ask for that.
- **Text replies only.** Teams has no voice message a bot can post, so a reply is never spoken, whatever `BRIDGE_VOICE_REPLY` says, and `/voice on` answers that this bot replies in text.

**How to send a recording.** Record it with any app (the phone's voice recorder, for example), then attach the file in the 1:1 chat with the bot: the paperclip on desktop, the **+** and file picker on mobile. You can type a message with it, such as "answer in French"; the bridge passes that on after the recording. The file stays in your OneDrive (Microsoft Teams Chat Files); the bridge reads it once and keeps nothing.

Formats taken: `m4a`, `mp4`, `mp3`, `wav`, `ogg`, `webm` and `flac`, the ones loomcycle transcribes without a converter. Any other file is a document (above) with `BRIDGE_FILES=1`, and ignored without it.

**What loomcycle needs:**

- A transcription engine (`transcription.stt:`, loomcycle's CONFIGURATION guide). No speech engine is needed: Teams never speaks.
- The transcription grant on each user's token. A bridge with a loomcycle token asks for it when it mints. For a gate bridge, add `surfaces: [transcription]` to the gate's `issued_token` in loomcycle (beside `agents` and `overrides`); without it the gate grants none, and a recording gets "I can only read text for now."
- `BRIDGE_RATE_PER_MINUTE`: each recording costs one extra start (the transcription), so raise it a little when voice is on.

### Downloads

Teams gives the bot a link to each attached file that works for only a few minutes, so the bridge fetches it at once, the same way for documents and recordings:

- **A credential only where the link needs one.** Until 2027-03-01 every link carries a `tempauth` query and works without a token: the bridge sends no credential and no header of its own. A link without `tempauth` (every link from 2027-03-01) is fetched with the bot's own file token ([below](#the-file-token-from-2027-03-01)), for that link's host only, and only after the host passed `TEAMS_BRIDGE_FILE_HOSTS` and the link proved to be in a OneDrive personal site (a host whose first label ends in `-my`, such as `contoso-my.sharepoint.com`, and a path under `/personal/`). Any other link without `tempauth`, a team site's included, is refused with reason `host` before a token is asked for. The token goes with the first request alone: every redirect is followed without it.
- **Only from SharePoint hosts:** the link, and every redirect it takes, must be `https` on a host ending in one of `TEAMS_BRIDGE_FILE_HOSTS` (default `.sharepoint.com`, which covers OneDrive's `<tenant>-my.sharepoint.com`). Add a sovereign cloud's suffix (`.sharepoint.us`, `.sharepoint.de`, `.sharepoint.cn`) if your tenant uses one. Anything else is refused.
- **Capped while reading:** Teams states no size up front, so the download stops once it passes `BRIDGE_FILES_MAX_BYTES` for a document (default 20 MiB) or `BRIDGE_VOICE_MAX_BYTES` for a recording (default 8 MB). `BRIDGE_VOICE_MAX_SECONDS` cannot apply, since Teams gives no length either; loomcycle's own limit on the minutes one transcription may take still does.
- A sign-in page or an error status instead of the file is refused.

When a download is refused, the person hears "I couldn't download that file. Please send it again." (for a recording, "I couldn't download that voice message. Please try again or type it.") and the bridge logs `teams file download refused` with the reason (`host`, `redirect`, `status`, `type`, `size` or `token`), the host or HTTP status where there is one, and never the link or a token. Reason `token` means a link without `tempauth` and no file token: `cause` is `none` (no certificate, managed identity or federated credential configured), `status` (the token endpoint refused, with its `status`), `response` or `failed`.

Not yet checked against a live tenant (RFC GA P0): the exact host Teams' `downloadUrl` uses, whether it redirects, and how long it lives. If files are refused with reason `host` or `redirect`, the logged host tells you what to add to `TEAMS_BRIDGE_FILE_HOSTS`.

### The file token from 2027-03-01

Microsoft ("Send and receive files", updated 2026-11-20): until March 1, 2027, file URLs have a `tempauth` query parameter and work without a token. From then on a bot sends its own access token with every file download, requested for `https://<the URL's host>/.default` with the bot's app id, and **a token acquired with a client secret gets HTTP 401**. It must come from a certificate, a federated identity credential or a managed identity (RFC HF §3.7).

The bridge takes the file token from the first of:

| Credential | Configure | Notes |
|---|---|---|
| Certificate | `CLIENT_CERTIFICATE_PATH`: a PEM file holding the certificate uploaded to the bot's Entra app (**Certificates & secrets → Certificates**) and its unencrypted RSA private key. Needs `CLIENT_ID` and `TENANT_ID`. | Used for file downloads only; the bot's own traffic keeps `CLIENT_SECRET` if one is set. Keep the file readable by the bridge alone (mode 0600, a mounted secret). |
| Managed identity or federated credential | No `CLIENT_SECRET`; `CLIENT_ID` (and `TENANT_ID`); for a federated credential also `MANAGED_IDENTITY_CLIENT_ID` (`system`, or a user-assigned identity's client id), the Teams SDK's own variable. | The SDK then uses it for everything, the bot's own traffic included, so it suits a bot hosted in Azure. |

With only `CLIENT_SECRET`, there is no file token, and with files or voice on the bridge warns at start: `files and recordings will fail from 2027-03-01: configure a certificate or federated credential (RFC HF)`. Until then links carry `tempauth` and keep working.

**The manifest permission, from 2027-01-15.** Teams then expects a bot that downloads files to declare the resource-specific permission `File.ReadSelected.User`, and refuses it in a manifest before that date, so `appPackage/manifest.json` does not carry it yet. On or after 2027-01-15, add this top-level key to the manifest, raise its `version`, and upload the app package again:

```json
"authorization": {
  "permissions": {
    "resourceSpecific": [{ "name": "File.ReadSelected.User", "type": "Application" }]
  }
}
```

### What Teams sent

When a person attaches a file and the agent never gets it, look at what Teams delivered:

- **Always:** a message whose attachments hold no file the bot can download (only `application/vnd.microsoft.teams.file.download.info` is one) logs `teams message carried no file` with the conversation, its type, the attachment count and their content types. It is logged when the message has no text, or has an attachment other than the `text/html` body Teams adds to a formatted message. No names, links or content.
- **`TEAMS_BRIDGE_DUMP_ACTIVITIES=1`:** every inbound activity that passes the Bot Connector check, of any type (a `messageUpdate` or an `invoke` too, not only messages), is logged as `teams activity` with the whole activity. Every URL keeps its host and path and loses its query and fragment (a `downloadUrl`'s query is a credential while it lives), fields named like a credential (`token`, `secret`, `authorization`, `tempauth`, ...) are replaced, and strings over 2000 characters are cut. What people write, their names and file names are kept: turn it on to look, then off. The bridge warns at start while it is on.

Use the dump rather than `LOG_LEVEL=debug`: at debug level the Teams SDK logs whole activity bodies unredacted, and for a working file that includes the `downloadUrl` with its credential.

### When Teams sends no file: the Graph fallback

On some tenants Teams delivers a file attached in a 1:1 chat as a message with an empty `text/html` attachment (or only the caption) and no `file.download.info` at all, so the bot has no link to fetch (RFC HF). The file is still in the sender's OneDrive, and the bridge can read the message through Microsoft Graph to find it. It is off by default; `BRIDGE_FILES=1` is needed for any of it.

| Variable | Meaning | Default |
|---|---|---|
| `TEAMS_BRIDGE_FILES_GRAPH` | `off`, or `app` (app-only Graph, below). `delegated` (each person signs in once) is planned and refused for now | `off` |
| `TEAMS_BRIDGE_APP_ID` | The Teams app's manifest `id`, a GUID: how the bridge finds the person's chat with the bot in Graph | `CLIENT_ID` (the shipped manifest's `id` is `BOT_ID`) |
| `TEAMS_BRIDGE_NO_FILE_HINT` | Added to the reply when a message looked like a file and none came, e.g. where else to send it | unset |

**With Graph off**, a message with no text and only an empty `text/html` attachment gets one reply: "I didn't receive a file. Teams didn't pass it to me." followed by `TEAMS_BRIDGE_NO_FILE_HINT` when set. The `teams message carried no file` log line stays.

**With `TEAMS_BRIDGE_FILES_GRAPH=app`**, every 1:1 message that came without `file.download.info` is looked up in Graph before it reaches the agent, with or without text, since a caption leaves no trace of the file. The lookup waits at most 5 seconds; a failure or a timeout logs `teams graph lookup failed` with a reason (`token`, `chat`, `sender`, `status` with the HTTP status, `timeout`) and the text goes on alone. For each message the bridge:

1. finds the person's chat with the bot once per conversation (kept in memory): the app installed for the person, filtered by `TEAMS_BRIDGE_APP_ID`, then its `chat`, which must be a one-on-one chat;
2. reads the message by the activity's id, re-reading briefly while Graph does not have it yet;
3. refuses a message whose Graph sender is not the activity's sender;
4. takes its `reference` attachments (name and OneDrive link) as files, which then go through `BRIDGE_FILES_TYPES`, the byte check and the size cap exactly as attached files do, with the text in the same message;
5. before downloading anything, reads the file's metadata through Graph's `shares` endpoint and the sender's own OneDrive (`/users/{id}/drive`, once per sender), and takes the file **only when it is in the sender's own OneDrive**, where Teams keeps a file attached from a device ("Microsoft Teams Chat Files");
6. downloads it by item (`/drives/{drive}/items/{item}/content`) with the app's Graph token, and follows Graph's redirect to SharePoint with **no** credential, only to a host in `TEAMS_BRIDGE_FILE_HOSTS` (the file's OneDrive link must be on one too). The Graph token goes to the Graph host only (`graph.microsoft.com`, or a sovereign cloud's, from the Teams SDK's cloud setting).

A message that still yields no file and looked like one gets the reply above.

**Why the OneDrive check.** The app token can read every file in the tenant, and a person can post a chat message through Graph themselves with a link to any file, one they cannot open included. Without the check the bot would fetch that file for them. So in app mode a file attached from SharePoint or a team site (anything outside the sender's own OneDrive) is refused: the person hears "I couldn't download that file. Please send it again." and the bridge logs `teams file download refused` with reason `not_sender_drive`, no name or link. Delegated mode (planned) will rely on Graph enforcing the person's own access instead.

**What app-only mode needs.** Application permissions on the bot's Entra app, with admin consent. They are **tenant-wide**: the bot's app can then read every chat message and every file in the tenant, not only those sent to the bot. Grant them only where your tenant's policy accepts that; delegated mode (planned), where each person signs in once and the bot reaches only their own chat and files, is the narrower choice.

| For | Application permission |
|---|---|
| Reading the message | `Chat.Read.All` |
| Finding the person's chat with the bot | `TeamsAppInstallation.ReadForUser.All` |
| The file's metadata, the sender's OneDrive id, and the download | `Files.ReadWrite.All` (the `shares` endpoint lists no read-only permission; `/users/{id}/drive` takes `Files.Read.All` or above, so it is covered) |

The token is the Teams SDK's app Graph token (client credentials of `CLIENT_ID`), asked for the activity's tenant.

**Not yet verified on a live tenant (RFC HF P0):** the chat lookup through the installed app (and that the filter on the manifest id matches it), that a Teams activity id is the Graph message id, the shape of a file in a Graph chat message (`contentType: "reference"` with `contentUrl` and `name`), that a file attached from a device sits in the sender's own OneDrive drive, the content redirect, and whether narrower permissions (`Chat.Read.WhereInstalled`, `Files.Read.All`) suffice. Until then treat app mode as a preview, and use the dump and the log lines above to see what happens.

## Security

- **Inbound:** the Teams SDK validates the Bot Connector's signature on every request. The bridge always sets the SDK's unauthenticated mode explicitly from `BRIDGE_DEV`, so a stray `DANGEROUSLY_ALLOW_UNAUTHENTICATED_REQUESTS` cannot turn validation off.
- **Tenant fence:** activities from Entra tenants not in `TEAMS_BRIDGE_ALLOWED_TENANTS` get one refusal and never reach loomcycle.
- **Least privilege:** the bridge's own token never runs agents. A user's token is limited to one agent, every per-run override except `interactive` is locked, it is rate-limited, and it expires.
- **Attached files** (files or voice on): fetched from `TEAMS_BRIDGE_FILE_HOSTS` only, redirects included, and held in memory for one request. A link with `tempauth` goes with no credential; one without goes with the bot's file token, acquired only after its host passed the list, scoped to that host, sent on the first request only (never across a redirect), and never logged. The file token never comes from a client secret.
- **What the file token can reach** is whatever SharePoint permissions the bot's Entra app holds, so a link Teams did not really send (a crafted `file.download.info`) must not be able to turn the bot into a reader of other files. Two limits apply. First, the bridge sends the token only to a OneDrive personal-site link (`<tenant>-my.<suffix>/personal/...`), so a team-site file never gets it. Second, grant the app only the `File.ReadSelected.User` resource-specific permission (files people shared with the bot, [from 2027-01-15](#the-file-token-from-2027-03-01)) and **no SharePoint API application role** (`Sites.Read.All`, `Sites.ReadWrite.All` and the like, under *SharePoint* in the app's API permissions). The file token is issued for SharePoint itself, so it carries only those roles: the Microsoft Graph permissions the [Graph fallback](#when-teams-sends-no-file-the-graph-fallback) needs (`Files.ReadWrite.All` under *Microsoft Graph*) are not in it. The bridge does not check whose OneDrive a link names: it is the permission that keeps another person's files, never shared with the bot, out of reach. Inbound activities are Bot Connector-signed, which is what makes a crafted attachment hard to deliver in the first place. A document is put into the person's own run with the person's own token; the agent gets its inbox ref, never its bytes or link.
- **Graph fallback** (`TEAMS_BRIDGE_FILES_GRAPH=app`): tenant-wide application permissions, so grant them knowingly. The Graph token is sent to the Graph host only and never logged or passed to a run; a lookup runs only for a 1:1 message that passed the Bot Connector check and the tenant fence, only a message from that same sender is read further, and only a file in that sender's own OneDrive is downloaded. Graph responses are never logged or dumped.
- **Nothing at rest:** tokens are kept in memory. Logs carry conversation and run ids, never tokens, message text or agent output.
- **Output:** tool inputs and outputs are never sent to Teams; while the agent works, Teams sees the tool's name only.

## As a library

```ts
import { createTeamsBridge, loadConfig } from "@loomcycle/bridge-teams";

const bridge = createTeamsBridge(loadConfig(), { store: myRedisStore });
await bridge.start();
```

`ConversationStore` (from `@loomcycle/bridge-core`) is four methods (`get`, `put`, `delete`, `list`); implement it over Redis or Postgres to run several instances.

Unlike the webhook bridges, the Teams bridge serves no `GET /healthz`: the Teams SDK's server registers POST routes only.

## Development

```bash
(cd ../bridge-core && npm ci && npm run build)   # the core, depended on by path while unpublished
npm ci
npm run typecheck
npm test                          # unit tests
./test/integration/run.sh         # builds loomcycle from this repo and runs against it (Go required)
```

The integration suite boots loomcycle on the mock provider, runs conversations through the bridge with real per-user tokens, and drives the real Teams SDK app with posted activities and a fake Bot Connector.

For a real Teams test, expose the bridge with a tunnel (for example Dev Tunnels), point the Azure Bot's messaging endpoint at it, and sideload the app package.
