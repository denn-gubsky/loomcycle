# scripts/

## `js-packages.sh` — npm tasks across every JavaScript package

The repo has several independent JavaScript packages (`web/`, `packages/*`,
`adapters/ts`, `cloud-deployment/landing`). `scripts/js-packages.sh` runs one
npm task in each of them and prints a summary table:

```sh
scripts/js-packages.sh install               # npm ci everywhere (do this first)
scripts/js-packages.sh typecheck --keep-going
scripts/js-packages.sh test -p packages/bridge
scripts/js-packages.sh audit --keep-going    # npm audit --omit=dev --audit-level=high
scripts/js-packages.sh list                  # which packages are covered
```

The same commands are available as `make js-install`, `make js-build`,
`make js-test`, `make js-typecheck` and `make js-audit`; pass options through
`JS_ARGS`, e.g. `make js-test JS_ARGS="--keep-going -p web"`. `lint` and
`outdated` are available from the script. Run `scripts/js-packages.sh --help`
for every option.

- A package that does not define the script (`build`, `test`, `typecheck`,
  `lint`) is reported as `skip`, not a failure.
- Without `--keep-going` the run stops at the first failing package; with it,
  every package runs and the exit status is non-zero if any failed.
- `web/` compiles `packages/{library,explorer,memory-view,def-fields}` from
  source, so those need `install` before `web/` can be checked on its own.
- `build` on `web/` writes into `internal/webui/dist/` without clearing old
  assets; to build the binary's embedded UI, keep using `make build-ui`.

### Adding a package

There is no list to edit. The package set is derived from the tracked
lockfiles (`git ls-files '*package-lock.json'`), so a new package is covered as
soon as its `package-lock.json` is committed. Every run warns about a tracked
`package.json` with no tracked lockfile, since that package would otherwise be
left out silently. To leave a package out on purpose, add its path to
`EXCLUDE` in the script, with the reason.

### Why not an npm workspace

Each package keeps its own lockfile on purpose. A package that depends on a
sibling (`@loomcycle/client`, `@loomcycle/bridge-core`, ...) consumes it by its
published npm version, never its source. A workspace would symlink the
siblings together, so local runs and CI would test a dependency graph that npm
users never install.

## `ci/modules.sh` and `ci/in-modules.sh` — every in-tree Go module

Go commands run from the repository root see only the root module: `go list
./...`, `go test ./...` and `go vet ./...` stop at a directory holding its own
`go.mod`. RFC FE moves some packages into nested modules, so every Go tool here
visits each module instead.

The in-tree modules are the root module plus every module the root `go.mod`
replaces with a directory inside the tree (`replace <path> => ./<dir>`). They
are read from the root `go.mod` (`scripts/ci/gomodules`), so there is no list
to keep: the directive that puts a module into the binary is the one that
makes the tooling visit it. `deploy/builder` is a separate module the root does
not replace; it is not one of them (`security.yml` scans it on its own).

```sh
scripts/ci/modules.sh                      # one directory per line, root (".") first
scripts/ci/modules.sh -group PKG...        # "DIR PKG..." per module owning an import path
scripts/ci/in-modules.sh go vet ./...      # run in every module
scripts/ci/in-modules.sh go test -race -- PKG...   # each module with its own packages
```

`in-modules.sh` runs every module even when one fails, and exits non-zero if
any did. A nested module's packages are run from that module's directory,
where its own `go.mod` and `go.sum` apply. `make test`, `make test-pg`,
`make vet`, `make tidy-all`, CI's Go checks (tidy, verify, vet, build) and its
test steps go through it; `gofmt -l .` walks files, so it already covers every module.

`internal/archtest` reads the import graph of every in-tree module and fails
on a `go.mod` anywhere in the tree that is neither the root's, an in-tree
module's, nor listed in its `standaloneModules`, so a module cannot be added
without being wired or declared. Each nested module is also listed in one
layer (`modules` in `internal/archtest/layers.go`), and its packages must sit
in that layer or below.

## `ci/select-packages.sh` and `ci/affected` — which Go tests a pull request runs

A pull request runs only the Go tests that can see its change. `main` runs
everything, so a test the selection misses still fails there.

```sh
scripts/ci/select-packages.sh ./...                 # what CI would run (all, outside a PR)
scripts/ci/select-packages.sh ./... "./internal/api/http/"   # less what a pattern names
GITHUB_EVENT_NAME=pull_request scripts/ci/select-packages.sh ./...
go run ./scripts/ci/affected -base "$(git merge-base origin/main HEAD)" -explain  # your commits, with reasons
```

`affected` selects a package with tests when a changed file is in the package
itself, in a package its test binary imports (test-only imports included), or
under a path its tests open with a relative literal such as
`"../../adapters/ts/src/types.ts"` or `filepath.Join("..", "..", "bundles")`.
Those paths are read from the test files, so there is no list to keep up to
date. A change confined to `_test.go` files selects only its own package.
Any in-tree module's `go.mod` or `go.sum`, `.github/workflows/ci.yml` and the
selector itself (with `scripts/ci/gomodules`) select every package.

The graph spans every in-tree module, each listed from its own directory; a
change to a nested module selects its own tests and the root packages that
import it. The `WITHIN` and `EXCLUDE` patterns are matched as `go list`
matches them, but over every module's packages, so `./...` names a nested
module's packages too. Outside a pull request, `select-packages.sh` prints
everything the patterns name (`affected -all`). Run its output with
`in-modules.sh`, as CI does.

The blind spot is a test that builds a path to another directory at run time
from something other than literals: write such paths as literals, or accept
that a pull request will not catch that test failing and `main` will.

## `ci/split-tests.sh` — one package's tests across several runners

`internal/api/http` and `internal/tools/builtin` each hold ~2,100 tests that
run one after another, so each sets the race job's length on its own. CI runs
each in parts on separate runners (`api/http` in thirds, `builtin` in halves):

```sh
scripts/ci/split-tests.sh ./internal/api/http/ 1/2 -race   # prints a -run regexp
```

The package may be an import path (what `select-packages.sh` prints), listed
from the directory of the module that owns it, or a relative path.

The names come from `go test -list` with the run's own build flags, sorted and
dealt round-robin, so every test lands in exactly one half and a new test needs
no list. To split another package, add its parts to the `go-race` matrix in
`.github/workflows/ci.yml` and its path to the `rest` shard's `without`.

## `ci/changes.sh` — which CI jobs a pull request needs

The `changes` job runs this first and every other job is skipped when its part
of the tree is untouched: `web` for the Web UI and the packages it compiles
from source, `ts` and `python` for the adapters, `bridges` for the bridge
packages and their image, `proto` for the proto and its generated stubs,
`runtime` for the code-js runtime suites. `go` is true when
`scripts/ci/affected` finds any Go test that can see the change, which
includes non-Go files that Go tests read (the TS adapter, the proto, `web/`).
A docs-only pull request runs nothing but this job. On `main` every job runs.

```sh
GITHUB_EVENT_NAME=pull_request CI_DIFF_BASE=abc123^ CI_DIFF_HEAD=abc123 scripts/ci/changes.sh
```
