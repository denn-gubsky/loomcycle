# Forge webhooks: GitHub, Gitea and GitLab

A forge (GitHub, Gitea, GitLab) sends one kind of request for every event — a push, a ping, a comment, a star — and signs the body, not the header that names the event. This guide sets up a loomcycle webhook that starts an agent run for the events you want, and answers the rest itself.

## Why the receiver, and not your agent

You could point a forge at a plain webhook and let the agent decide what to do with each delivery. Every delivery would then start a run — a ping, a branch deletion, a push to a tag — and the run would be the first place anything looks at it. A run is the expensive place to say no: it takes a concurrency slot, a model call, maybe a GPU. And a rule each agent writes for itself is a rule each gets slightly wrong.

So the webhook definition says which deliveries start a run, and the receiver answers every other one from the signed body before anything starts: a ping gets `200`, an event you did not list `202 ignored`, a push to a tag `202 filtered`, the same push sent twice a duplicate.

## A GitHub push webhook, end to end

### 1. Store the signing secret as a credential

```json
{"op": "create", "scope": "tenant", "name": "acme-app-hook", "value": "<the secret you give GitHub>", "allowed_urls": []}
```

with `CredentialDef`. `allowed_urls: []` stores a value that is compared, never sent anywhere. Rotating it later is another `create` with the same name: the receiver reads it on every delivery, so no restart.

### 2. Create the definition

```json
{
  "op": "create",
  "name": "acme-app-push",
  "overlay": {
    "enabled": true,
    "delivery": "spawn",
    "agent": "code-reviewer",
    "provider": "github",
    "events": ["push"],
    "auth": {"secret": "$cred:acme-app-hook"},
    "metadata": {"repo": "acme/app"},
    "filters": [
      {"path": "$.ref", "op": "glob", "value": "refs/heads/*"},
      {"path": "$.deleted", "op": "eq", "value": false},
      {"path": "$.repository.full_name", "op": "eq", "value_from": "metadata.repo"}
    ],
    "dedup_key": ["$.repository.id", "$.ref", "$.after"],
    "user_bearer": "$cred:acme-app-mcp-token"
  }
}
```

with `WebhookDef`. Line by line:

- **`provider: github`** fills the signature header (`X-Hub-Signature-256`), the event header (`X-GitHub-Event`) and the delivery-id header (`X-GitHub-Delivery`). Anything you set yourself wins.
- **`events: [push]`** — only a push starts anything. A `ping` is answered `200`, any other event `202 ignored`.
- The event header is not signed, so **a push counts only when the signed body has a push's shape** — the provider knows it. A push body re-sent as `pull_request` is refused `400 event_shape_mismatch`.
- **`filters`** — a push to a tag, a branch deletion and a push from another repository are answered `202 filtered`, naming the filter that missed. `value_from: metadata.repo` reads the definition's own metadata, so one definition shape serves every repository.
- **`dedup_key`** — one pushed head is one run, whatever the delivery id and however the bytes differ, on every replica and across restarts, for 7 days (`dedup_ttl_seconds`, up to 90). If that run ends `failed`, the forge's redelivery starts a new one.
- **`user_bearer`** — what `${run.user_bearer}` becomes in the headers of the MCP servers the run calls: a tenant credential, resolved for each MCP request and that server's URL, so a credential restricted by `allowed_urls` goes only to its server.

### 3. Point GitHub at it

In the repository's webhook settings: Payload URL `https://<loomcycle>/v1/_webhooks/<tenant>/acme-app-push`, content type `application/json`, the secret from step 1, and the `push` event.

### 4. Try it without starting anything

`POST /v1/_webhooks/acme-app-push/test` (admin) with a sample body and headers reports what a real delivery would do — `would_accept`, or the verdict (`ping`, `ignored`, `filtered` with the filter's index, `rejected_shape`, …) — and creates nothing.

## Gitea and GitLab

- **Gitea:** `provider: gitea`. The signature is bare hex in `X-Gitea-Signature`; events come in `X-Gitea-Event`. Gitea sends a comment on a pull request as `pull_request_comment` and on an issue as `issue_comment`, with one body; the preset tells them apart by `is_pull`.
- **GitLab:** `provider: gitlab`. GitLab signs nothing: it sends a shared token in `X-Gitlab-Token`, compared in constant time, so leave `auth.kind` to the provider and store the token as the credential. Event names are listed by their spelling without ` Hook`: `Push Hook` is `push`, `Merge Request Hook` is `merge_request`. `dedup_key` still gives one run per event, but against someone who captured a request it protects nothing — whoever holds the request holds the token.

## What the run receives

A run started by a webhook that lists `events` gets, beside the definition's own metadata:

```json
{"repo": "acme/app", "webhook": {"provider": "github", "event": "push", "delivery_id": "72d3162e-cc78-11e3-81ab-4c9367dc0958"}}
```

It is trusted, as the definition's metadata is: none of it is read from the body. `event` is one the definition lists, put there only after the body had its shape. `delivery_id` is a label the forge does not sign; it is passed only as a UUID, a hex digest or a number, so a sender cannot spell an instruction with it. A code-js agent reads all this at `input.metadata.webhook`. The body itself reaches the run as untrusted input.

## Operating it

- **How deliveries were answered:** `WebhookDef op=get` returns `status.deliveries` — accepted, duplicate, ping, ignored, filtered, and refused by reason (signature, event, shape, body, rate, …) — every replica's counts, up to 30 seconds behind. An admin also has `GET /v1/_webhooks/{name}/delivery-counts`.
- **The last few deliveries** on one replica, with their verdicts and run ids: `GET /v1/_webhooks/{name}/recent-deliveries`.
- **A `404` from a webhook you created means a wrong secret or signature.** A failed signature gets the same answer as a name that does not exist, so a sender cannot probe for names; the delivery counts (refused: signature), the recent deliveries (`rejected_sig`) and the server log say why. A secret that does not resolve answers `503 {"error":"unavailable"}`, and only the log and the delivery log name it.
- **Rotating the secret:** set `auth.previous_secret` to the old credential and `auth.secret` to the new one in a fork; a delivery signed with either is accepted. Fork again without `previous_secret` when the forge has the new one.
- **The console** (Integrations → Webhooks) edits every field above and shows the delivery counts.

## What this does not protect against

- **A body-only signature carries no time.** After a `dedup_key` expires, a captured delivery with altered unsigned headers is a new event. If that matters, check the event against your own state — for a push, that its head is still the branch's newest.
- **A shape tells events apart only by what their bodies hold.** The presets give shapes only to events no other event's body is known to meet. For an event the provider has no shape for, write one in `event_shapes` with `!path` (a field the body must not hold) or `path == "value"` where a path alone is not enough.
- **GitLab's token is a password, not a signature.** Restrict who can read it, and rotate it like one.
