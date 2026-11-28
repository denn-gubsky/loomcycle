# Example workspace session images

[`Dockerfile.dev`](Dockerfile.dev) is an **example** session image for a workspace
worker on the container engine that builds Go, C and C++: the published
`denngubsky/loomcycle-workspace` image plus Go, gcc/g++, make, cmake and
pkg-config from Debian's archive (RFC HH). loomcycle does not publish it (RFC HH
D5). Copy it, add what your builds need, and build it yourself.

A session's root is read-only and it runs as a non-root user with no network but
the egress routes, so an agent cannot install a compiler at run time. Whatever a
build needs from the system has to be in the image.

## Build it

On the host whose container daemon runs the sessions (the worker hands the image
name to that daemon, which uses the local image and pulls only when it has none):

```sh
docker build -f deploy/workspace-images/Dockerfile.dev \
  -t loomcycle-workspace-dev:1.173.0 deploy/workspace-images
```

Keep the base on the runtime's own release:
`--build-arg LOOMCYCLE_WORKSPACE_VERSION=<version>`. For a dev image that can
also take screenshots, add
`--build-arg LOOMCYCLE_WORKSPACE_IMAGE=denngubsky/loomcycle-workspace-browser`.

## Run a worker on it

A second workspace worker beside the general one, with its own `--id`, workspace
root and egress directory, and the toolchains to probe for:

```sh
LOOMCYCLE_WORKER_DISPATCHER=runtime.internal:7788 \
LOOMCYCLE_WORKER_TOKEN=<a workers:attach token> \
LOOMCYCLE_WORKSPACE_ROOT=/var/lib/loomcycle/workspaces-dev \
LOOMCYCLE_WORKSPACE_ENGINE=container \
LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE=loomcycle-workspace-dev:1.173.0 \
LOOMCYCLE_WORKSPACE_TOOLCHAINS=python,node,go,c,cpp,cmake,make \
loomcycle worker --role workspace --id ws-dev
```

The worker reports only the toolchains its image really has, and logs a warning
for each declared one it does not find. An agent then asks for one with
`Workspace op=open` and `toolchains: ["cpp", "cmake"]`, or is pinned to them with
`workspace.toolchains` in its grant. `Workspace op=toolchains` lists what is on
offer.

The guides: [`docs/WORKSPACES.md`](../../docs/WORKSPACES.md) "Toolchains";
[`deploy/truenas/INSTALL.md`](../truenas/INSTALL.md) and
[`cloud-deployment/INSTALL.md`](../../cloud-deployment/INSTALL.md), "A worker for
C++ and Go builds".
