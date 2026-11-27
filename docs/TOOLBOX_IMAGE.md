# The `loomcycle-toolbox` image

> **Removed in v1.172.0.** The `loomcycle-toolbox` image, and the `Bash` and `Bashbox` tools it existed to serve, were removed in v1.172.0 (RFC GW P15). This page stays for one release so old links resolve.

Use the distroless `denngubsky/loomcycle` image, and run commands in a workspace on a workspace worker: a process outside loomcycle's container that holds no credential. The published session image `denngubsky/loomcycle-workspace` carries Python and Node; build on it for more. `denngubsky/loomcycle-ws-worker` is a worker that drives the host's Docker and starts that session image for each workspace.

- [WORKSPACES.md](WORKSPACES.md) is the guide.
- [MIGRATING-FROM-BASH.md](MIGRATING-FROM-BASH.md#images) maps the image across, and its "Upgrading to v1.172.0" section lists every line to change.
- Old `loomcycle-toolbox` tags stay published; no new tag is pushed after v1.171.x.
