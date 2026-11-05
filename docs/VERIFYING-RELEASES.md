# Verifying a loomcycle release

A checksum only proves that a download matches the `SHA256SUMS` file next to
it. Anyone who can replace the archive on a mirror, in a cache or in a
registry can replace that file too. So every release also carries proof of
**who built it and from what**, checkable without trusting the place you
downloaded it from:

- a **signature** over `SHA256SUMS` and over every container image, made with
  [cosign](https://github.com/sigstore/cosign) "keyless": there is no signing
  key to leak or rotate. The signing certificate is issued to this
  repository's release workflow, at the tag, through GitHub's OIDC token, and
  the signature is recorded in the public Sigstore transparency log;
- a **build provenance attestation** (SLSA v1) for every archive, SBOM and
  image, stored with GitHub and checked with `gh attestation verify`. It names
  the repository, the workflow, the commit and the tag that produced the file;
- an **SBOM** (SPDX JSON) per archive, listing every Go module in the binary;
- a **third-party license bundle** (`THIRD_PARTY_LICENSES`) inside every
  archive: the license texts of the Go modules linked into the binary and of
  the npm packages bundled into its embedded Web UI.

Releases up to and including v1.141.0 predate this and carry none of it.

You need [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
v3 (the release signs with it, in the Sigstore bundle format; older cosign
releases may not read those bundles) and the [GitHub CLI](https://cli.github.com/)
(`gh`) for the attestations.

## Binaries and archives

Each release attaches, per platform, `loomcycle-<os>-<arch>.tar.gz` and its
SBOM `loomcycle-<os>-<arch>.tar.gz.sbom.json`, plus `SHA256SUMS` (which lists
both) and `SHA256SUMS.sigstore.json` (the signature, its certificate and the
transparency-log proof in one file).

```sh
VERSION=1.142.0        # the release you are checking, without the leading v
FILE=loomcycle-linux-amd64.tar.gz
BASE=https://github.com/denn-gubsky/loomcycle/releases/download/v$VERSION
curl -fsSLO "$BASE/$FILE" -O "$BASE/SHA256SUMS" -O "$BASE/SHA256SUMS.sigstore.json"

# 1. SHA256SUMS was signed by the release workflow at this tag.
cosign verify-blob SHA256SUMS \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/denn-gubsky/loomcycle/.github/workflows/release.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 2. The archive is the one SHA256SUMS lists.
grep "  $FILE\$" SHA256SUMS | sha256sum -c -          # macOS: shasum -a 256 -c -

# 3. GitHub holds a build provenance attestation for it from that workflow.
gh attestation verify "$FILE" \
  --repo denn-gubsky/loomcycle \
  --signer-workflow denn-gubsky/loomcycle/.github/workflows/release.yml
```

Step 1 and 2 together prove the archive is what the release workflow built;
step 3 proves the same through an independent record and adds what it was
built from (`gh attestation verify --format json` prints the commit and the
workflow run). The SBOM verifies the same way: it is listed in `SHA256SUMS` and
has its own attestation.

## Container images

Every image is published to Docker Hub (`denngubsky/...`) and to the GHCR
mirror (`ghcr.io/denn-gubsky/...`). Both copies are signed. A version tag and
`latest` are signed by digest, so the signature follows the image, not the
tag.

```sh
VERSION=1.142.0
IMAGE=denngubsky/loomcycle      # or ghcr.io/denn-gubsky/loomcycle, -toolbox, -media, -browser

cosign verify "$IMAGE:$VERSION" \
  --certificate-identity "https://github.com/denn-gubsky/loomcycle/.github/workflows/release.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

gh attestation verify "oci://$IMAGE:$VERSION" \
  --repo denn-gubsky/loomcycle \
  --signer-workflow denn-gubsky/loomcycle/.github/workflows/release.yml
```

`gh attestation verify` looks the attestation up on GitHub by the image's
digest, so it works for either registry's copy (log in to the registry first
if it asks). The attestation is also pushed next to the image in the registry;
add `--bundle-from-oci` to read it from there instead.

Different images come from different workflows, and the certificate names the
workflow and the git ref it ran at:

| Image | Workflow | Ref in the certificate |
|---|---|---|
| `loomcycle`, `loomcycle-toolbox`, `loomcycle-media`, `loomcycle-browser` (minor and major releases) | `release.yml` | `refs/tags/vX.Y.Z` |
| `loomcycle-browser` (patch releases) | `release.yml` | `refs/tags/vX.Y.Z` |
| `loomcycle-builder`, `loomcycle-builder-docker`, `loomcycle-sandbox-session` | `publish-sandbox-images.yml` | `refs/tags/vX.Y.0`, or the branch a manual run used |
| `loomcycle-bridge-<name>` | `publish-bridge-images.yml` | `refs/tags/bridge-<name>-vX.Y.Z`, or the branch a manual run used |

For an image a manual run published, match the workflow and not the ref:

```sh
cosign verify ghcr.io/denn-gubsky/loomcycle-bridge-slack:0.3.0 \
  --certificate-identity-regexp '^https://github\.com/denn-gubsky/loomcycle/\.github/workflows/publish-bridge-images\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## npm packages

`@loomcycle/client`, the bridges and the UI packages are published with npm
provenance (`@loomcycle/client` already was; the others from their first
version published after v1.141.0): the package page on npmjs.com shows where and how each version was
built, and the provenance is signed through the same GitHub OIDC token. After
installing, check every package in your tree:

```sh
npm audit signatures
```

## The Python adapter

`loomcycle` on PyPI is published through PyPI Trusted Publishing with PEP 740
attestations: each wheel and sdist carries a Sigstore-signed statement that
this repository's `publish-python-adapter.yml` built it. PyPI shows them on
the file's page; [`pypi-attestations`](https://pypi.org/project/pypi-attestations/)
checks them from the command line.

## What is not covered

- The standalone Web UI bundles (`webui-v*` releases) are not signed yet; check
  them against the digest in their release notes, which the server also
  enforces through `LOOMCYCLE_WEBUI_BUNDLE_SHA256`.
- Container images carry no SBOM of their own yet. The binary inside is the one
  the archive SBOMs describe.
- Homebrew installs the release archives; `brew` checks their SHA-256 against
  the formula, which the release workflow writes. To check the signature too,
  verify the archive as above.
