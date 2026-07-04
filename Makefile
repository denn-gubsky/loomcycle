# loomcycle Makefile — dev-loop helpers.
#
# Targets fall into two groups:
#   1. Build / test the runtime ("test", "build", "test-pg").
#   2. Bring up local dependencies for the test fixtures ("pg-up", "pg-down").
#
# CI sets LOOMCYCLE_TEST_PG_DSN to a service-container Postgres and
# skips the pg-up/pg-down steps. Local devs run `make pg-up` once per
# session, then `make test` covers everything (Postgres tests skip
# automatically when the env var is unset).

# Container name + image used by pg-up. Override via env if needed:
#   PG_CONTAINER=my-pg make pg-up
PG_CONTAINER ?= loomcycle-test-pg
PG_IMAGE     ?= postgres:16-alpine
PG_PORT      ?= 5432
PG_DATABASE  ?= loomcycle_test
PG_USER      ?= loomcycle
PG_PASSWORD  ?= loomcycle

PG_DSN := postgres://$(PG_USER):$(PG_PASSWORD)@127.0.0.1:$(PG_PORT)/$(PG_DATABASE)?sslmode=disable

.PHONY: help build build-ui build-all test test-pg pg-up pg-down pg-logs proto proto-deps python-proto python-test

help:
	@echo "loomcycle dev targets:"
	@echo "  build       — go build -o bin/loomcycle ./cmd/loomcycle (the runnable binary)"
	@echo "  build-ui    — npm ci + npm build for web/ (output → internal/webui/dist/)"
	@echo "  build-all   — build-ui + build → ./bin/loomcycle with the latest UI embedded (use this to deploy)"
	@echo "  test        — go test ./... (Postgres tests skip without LOOMCYCLE_TEST_PG_DSN)"
	@echo "  test-pg     — go test ./... with LOOMCYCLE_TEST_PG_DSN set against the local fixture"
	@echo "  pg-up       — start an ephemeral Postgres container for the test fixture"
	@echo "  pg-down     — stop + remove the test fixture container"
	@echo "  pg-logs     — tail the test fixture container's logs"
	@echo "  proto       — regenerate Go gRPC stubs from proto/loomcycle.proto"
	@echo "  proto-deps  — install the Go protoc plugins (one-time)"
	@echo ""
	@echo "Local DSN: $(PG_DSN)"

# Produce the runnable binary at ./bin/loomcycle. This MUST be the
# `-o bin/loomcycle ./cmd/loomcycle` form: `go build ./...` is only a
# compile check and writes NO executable (Go discards binaries when
# building multiple packages), so the old target silently left a stale
# ./bin/loomcycle in place — deploys shipped old code. The all-packages
# compile check lives in `go vet ./...` (run in CI alongside this).
build:
	go build -o bin/loomcycle ./cmd/loomcycle

build-ui:
	# Build the React SPA into internal/webui/dist/. The dist/ tree is
	# wiped first so old content-hashed assets don't accumulate, then
	# the .gitkeep placeholder is restored so go:embed always has at
	# least one matching file (a fresh checkout without npm-build still
	# compiles Go).
	#
	# `npm ci` (not `npm install`) so the lock file is treated as
	# authoritative AND is never mutated. The release.yml goreleaser
	# job fails on a dirty working tree; `npm install` would touch
	# web/package-lock.json on any silent drift and break the release.
	rm -rf internal/webui/dist/*
	# @loomcycle/library (RFC AY) is consumed from SOURCE via a Vite alias, so the
	# web build compiles the package's TSX and must resolve its react / js-yaml /
	# @loomcycle/client imports. Those resolve from packages/library/node_modules,
	# which is gitignored (absent on a fresh checkout / in CI) — install it first.
	cd packages/library && npm ci --silent
	cd web && npm ci --silent && npm run build
	touch internal/webui/dist/.gitkeep

build-all: build-ui build

test:
	go test ./...

test-pg:
	@echo "Running tests with LOOMCYCLE_TEST_PG_DSN=$(PG_DSN)"
	LOOMCYCLE_TEST_PG_DSN="$(PG_DSN)" go test ./...

# runtime-mock: the fast, deterministic runtime suites (live binary, mock
# provider — no real provider, no API key, no Postgres). Each prints
# PASS/FAIL and exits non-zero on failure. NOT part of `test` / CI: these
# boot the binary and one waits ~60s for a real cron fire. memory-vector is
# excluded here (needs Postgres + pgvector); run it via runtime-vector.
runtime-mock:
	@set -e; for s in schedules webhooks memory-core code-js; do \
		echo "=== runtime/$$s ==="; ./test/runtime/$$s/run.sh; \
	done; echo "=== all runtime-mock suites PASSED ==="

# runtime-codejs: the FUNCTIONAL synthetic code-js provider (RFC J) suite.
# CI-friendly — fully deterministic (no LLM, the provider runs operator JS via
# goja), no API key, no Postgres, no long cron waits — so the CI workflow runs
# this target. Validates the code-js loop+replay end-to-end and the Memory
# meta-tool op surface. The STRESS behaviours (concurrency, the iteration-cap
# exemption, the run-timeout-bounded runaway) live in runtime-stress, which CI
# does NOT run — see that target.
runtime-codejs:
	@set -e; for s in code-js; do \
		echo "=== runtime/$$s ==="; ./test/runtime/$$s/run.sh; \
	done; echo "=== all runtime-codejs suites PASSED ==="

# runtime-stress / runtime-soak: LOCAL-ONLY, on-demand. Load, stress, and
# soak / sustainability suites are deliberately kept OUT of CI — they are
# slower, concurrency- and timing-sensitive, and meant to run on the operator's
# own machine where load characteristics are controlled. CI runs only the fast,
# deterministic functional suites above.
runtime-stress:
	@set -e; for s in code-js-stress; do \
		echo "=== runtime/$$s ==="; ./test/runtime/$$s/run.sh; \
	done; echo "=== all runtime-stress suites PASSED (local-only; not run in CI) ==="

# runtime-vector: the Memory vector + dedup suite against Postgres+pgvector
# with the deterministic stub embedder. Provide LOOMCYCLE_TEST_PG_DSN
# pointing at a Postgres that has the `vector` extension (it SKIPs without).
runtime-vector:
	./test/runtime/memory-vector/run.sh

# runtime-soak: the 30-minute stability test (override duration with
# LOOMCYCLE_SOAK_SECONDS). On-demand only.
runtime-soak:
	./test/runtime/stability/run.sh

# pg-up: bring up an ephemeral Postgres container for the test suite.
# The data volume lives inside the container (no host-bind) so a
# `docker rm` after pg-down leaves no state behind. The exposed port
# is 5432 by default; override PG_PORT if it conflicts.
#
# Idempotent: if the container is already running, this prints a
# notice and exits 0. If it's stopped (after pg-down), `docker start`
# revives it.
pg-up:
	@if docker ps --format '{{.Names}}' | grep -q '^$(PG_CONTAINER)$$'; then \
		echo "$(PG_CONTAINER) already running"; \
	elif docker ps -a --format '{{.Names}}' | grep -q '^$(PG_CONTAINER)$$'; then \
		docker start $(PG_CONTAINER) >/dev/null && echo "$(PG_CONTAINER) restarted"; \
	else \
		docker run -d --name $(PG_CONTAINER) \
			-e POSTGRES_USER=$(PG_USER) \
			-e POSTGRES_PASSWORD=$(PG_PASSWORD) \
			-e POSTGRES_DB=$(PG_DATABASE) \
			-p $(PG_PORT):5432 \
			$(PG_IMAGE) >/dev/null && \
		echo "$(PG_CONTAINER) started on port $(PG_PORT)"; \
	fi
	@echo "Waiting for Postgres to accept connections..."
	@for i in $$(seq 1 30); do \
		if docker exec $(PG_CONTAINER) pg_isready -U $(PG_USER) -d $(PG_DATABASE) >/dev/null 2>&1; then \
			echo "Postgres ready"; \
			echo "Export LOOMCYCLE_TEST_PG_DSN=\"$(PG_DSN)\" to run the suite manually."; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "Postgres did not become ready in 30s; check 'make pg-logs'" && exit 1

pg-down:
	@if docker ps -a --format '{{.Names}}' | grep -q '^$(PG_CONTAINER)$$'; then \
		docker rm -f $(PG_CONTAINER) >/dev/null && echo "$(PG_CONTAINER) removed"; \
	else \
		echo "$(PG_CONTAINER) not running"; \
	fi

pg-logs:
	docker logs -f $(PG_CONTAINER)

# proto / gRPC codegen.
#
# We commit the generated *.pb.go files into the tree (alongside the
# proto, under internal/api/grpc/loomcyclepb/) so a fresh checkout
# builds without first running protoc. Re-run `make proto` whenever
# proto/loomcycle.proto changes.
PROTO_OUT_DIR := internal/api/grpc/loomcyclepb
proto:
	@if ! command -v protoc >/dev/null 2>&1; then \
		echo "protoc not found; install via your package manager (e.g. brew install protobuf)"; \
		exit 1; \
	fi
	@if ! command -v protoc-gen-go >/dev/null 2>&1; then \
		echo "protoc-gen-go not found; run 'make proto-deps' first"; \
		exit 1; \
	fi
	mkdir -p $(PROTO_OUT_DIR)
	protoc \
		--go_out=$(PROTO_OUT_DIR) \
		--go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_OUT_DIR) \
		--go-grpc_opt=paths=source_relative \
		--proto_path=proto \
		proto/loomcycle.proto
	@echo "regenerated $(PROTO_OUT_DIR)/*.pb.go"

proto-deps:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo ""
	@echo "Add $$(go env GOPATH)/bin to your PATH if it isn't already."

# Python adapter codegen + tests.
#
# python-proto regenerates the Python protobuf stubs into
# adapters/python/loomcycle/_generated/. Both files are committed so
# `pip install loomcycle` doesn't require a working protoc.
#
# python-test runs the adapter's pytest suite. Live-loomcycle
# integration tests skip without LOOMCYCLE_GRPC_ADDR set.
PY_VENV       := adapters/python/.venv
PY_PROTO_OUT  := adapters/python/loomcycle/_generated
python-proto:
	@if [ ! -x "$(PY_VENV)/bin/python" ]; then \
		echo "Python venv not found at $(PY_VENV); create it with:"; \
		echo "  python3 -m venv $(PY_VENV) && \\"; \
		echo "  $(PY_VENV)/bin/pip install -e adapters/python[dev]"; \
		exit 1; \
	fi
	mkdir -p $(PY_PROTO_OUT)
	$(PY_VENV)/bin/python -m grpc_tools.protoc \
		--python_out=$(PY_PROTO_OUT) \
		--grpc_python_out=$(PY_PROTO_OUT) \
		--proto_path=proto \
		proto/loomcycle.proto
	@# grpc_tools generates absolute imports (`import loomcycle_pb2`)
	@# rather than the relative form (`from . import loomcycle_pb2`)
	@# Python packages need. Patch the grpc stub so it's importable
	@# from the loomcycle._generated package without a sys.path hack.
	@sed -i '' 's/^import loomcycle_pb2 as loomcycle__pb2$$/from . import loomcycle_pb2 as loomcycle__pb2/' $(PY_PROTO_OUT)/loomcycle_pb2_grpc.py
	@touch $(PY_PROTO_OUT)/__init__.py
	@echo "regenerated $(PY_PROTO_OUT)/*.py"

python-test:
	$(PY_VENV)/bin/python -m pytest adapters/python/tests -v
