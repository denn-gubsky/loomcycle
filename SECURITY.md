# Security policy

## Supported versions

Security fixes land on `main` and ship in the next release of the **latest minor line** only. Older lines get no backports: upgrade to the latest release.

| Version | Supported |
| ------- | --------- |
| 1.131.x (latest minor) | Yes |
| Anything older | No |

The table moves with each minor release; the [releases page](https://github.com/denn-gubsky/loomcycle/releases) is authoritative. The same rule covers the adapters and packages published from this repo (`@loomcycle/client`, the `loomcycle` Python package, the `@loomcycle/*` npm packages, the Docker images): the latest published version is the supported one.

## Reporting a vulnerability

**Do not open a public issue, discussion or pull request for a vulnerability.**

Report it privately through GitHub Security Advisories: the **Report a vulnerability** button on the repository's Security tab, or directly at

<https://github.com/denn-gubsky/loomcycle/security/advisories/new>

The report and the discussion stay private between you and the maintainers until a fix is published.

If the **Report a vulnerability** button is not available, contact the maintainer privately through the email address on their GitHub profile. Never through a public issue.

Please include:

- the affected version (`loomcycle --version`) or commit, and how it was installed (Homebrew, Docker, binary, source);
- the relevant configuration: auth mode (open, shared token, per-principal tokens), enabled tools, volumes, storage backend. Redact every secret;
- the surface involved: HTTP/SSE, gRPC, MCP, Web UI, an adapter, a tool, a provider driver;
- steps to reproduce, ideally a minimal config plus the requests or prompt that trigger it;
- the impact as you see it: what an attacker controls, and what they gain.

## What to expect

These are targets, not guarantees:

- **Acknowledgement** within 3 business days.
- **Triage** (confirmed or not, severity, affected versions) within 10 business days.
- **Fix:** critical and high severity in the next patch release, as soon as a fix is verified; medium within about 30 days; low in a regular release.

We keep you informed while we work on it, credit you in the advisory unless you ask us not to, and publish a GitHub Security Advisory (with a CVE where one applies) when the fixed release is out.

## Coordinated disclosure

Please give us the time to ship a fix before you disclose publicly. We will agree a disclosure date with you; the default is the release of the fix, and at most 90 days after the report. If a vulnerability is already public or being exploited, tell us and we will move faster.

## Scope and threat model

In scope: the `loomcycle` binary and its embedded Web UI, the wire surfaces (HTTP/SSE, gRPC, the MCP server), the adapters and packages in this repo, and the Docker images and release artifacts built from it.

Some behaviour is a documented property of the posture an operator chose, not a vulnerability. Read these before reporting:

- **The `Bash` tool is not a sandbox.** When enabled it runs real host processes that can reach absolute paths and the network. Run loomcycle in a container or VM if `Bash` is exposed to untrusted prompts. See [`docs/TOOLS.md`](docs/TOOLS.md#built-ins).
- **Open mode is for localhost only.** With no `LOOMCYCLE_AUTH_TOKEN` and no minted tokens, every request is allowed as an admin. See [README: Tier 1](README.md#tier-1-zero-config-dev-open-mode-localhost) and [`docs/CONFIGURATION.md` §9b](docs/CONFIGURATION.md#9b-multi-tenant-authentication-rfc-l) for the authenticated modes.
- **Bashbox and gbash maturity.** `Bashbox` runs commands in gbash, an in-process sandbox with no OS process and no network; gbash is alpha and pinned. It is a strong layer for untrusted prompts; for mutually hostile tenants on one process, also run loomcycle in a container. Commands an operator allowlists for host fallback run on the real host. See [`docs/TOOLS.md`](docs/TOOLS.md#built-ins) and the `bashbox` help topic.
- **A private host vouched for by name trusts its DNS.** An entry in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST` that is a host name (suffix-matched) lets every outbound request loomcycle makes (the HTTP and WebFetch tools, MCP over HTTP, A2A, hook webhooks, remote memory and document peers, git sources) reach that name wherever it resolves: loopback, RFC 1918, CGNAT or ULA. Whoever controls the name's DNS, or can create a name under a suffix entry, can point it at another private service. A vouched name never reaches a link-local or cloud-metadata address; only a CIDR entry covering the address admits those. To trust no DNS, list private hosts by CIDR only. See [`docs/TOOLS.md`](docs/TOOLS.md#built-ins).
- **Operator-token hashes are peppered only with a secret.** A token row stores a hash, never the token. The pepper that stops a database writer from inserting a row for a token of their choosing is `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`, or one derived from `LOOMCYCLE_SECRET_KEY`. With neither, rows are hashed with no pepper and the server only warns at boot. Set `LOOMCYCLE_SECRET_KEY` on any deployment with minted tokens. See [`docs/CONFIGURATION.md`, Token pepper](docs/CONFIGURATION.md#token-pepper).
- **Volumes are the filesystem boundary.** An agent reaches the disk only through the volumes bound to it; `Bash` and Bashbox host-fallback commands are not confined by them. See [`docs/CONFIGURATION.md` §9d](docs/CONFIGURATION.md#9d-filesystem-volumes--per-agent-rorw-scopes-rfc-ah) and the `volumes` help topic.
- **The code sandbox sidecar** has its own isolation posture: [`docs/SANDBOX.md`](docs/SANDBOX.md#isolation-posture).

A way around one of these documented boundaries — a tenant reaching another tenant's data, a model widening an operator's allowlist, a Bashbox command escaping its volume — is in scope. So is anything that leaks a secret, a credential or a bearer token.

Out of scope: vulnerabilities in model providers, third-party MCP servers or other upstream services (report those to their owners), and findings that need an operator to have disabled a documented protection.

The [security highlights](README.md#security-highlights) in the README summarize the default-deny model.
