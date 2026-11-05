# Maintainers

| Name | GitHub | Scope |
|---|---|---|
| Dennis Gubsky | [@denn-gubsky](https://github.com/denn-gubsky) | Everything: runtime, adapters, packages, CI, releases, security response |

loomcycle has one maintainer today. That is a known risk for a project other
systems depend on: one person reviews every change, holds the release and
publishing credentials, and answers every security report. A second maintainer
will be added, and this file will name them when they are.

When there is a second maintainer:

- they join the security-sensitive entries in [`.github/CODEOWNERS`](.github/CODEOWNERS)
  first, so a change to a trust boundary (authentication, credentials,
  sandboxes, tenant-isolated storage, CI and release) always needs a review
  from someone other than its author;
- protection on `main` requires one approving review, including a code
  owner's (with one maintainer it cannot: an author cannot approve their own
  pull request);
- they are given access to private security advisories.

## Reporting a vulnerability

Not through this file or a public issue: see [`SECURITY.md`](SECURITY.md).

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md).
