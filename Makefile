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

.PHONY: help build test test-pg pg-up pg-down pg-logs proto proto-deps python-proto python-test

help:
	@echo "loomcycle dev targets:"
	@echo "  build       — go build ./..."
	@echo "  test        — go test ./... (Postgres tests skip without LOOMCYCLE_TEST_PG_DSN)"
	@echo "  test-pg     — go test ./... with LOOMCYCLE_TEST_PG_DSN set against the local fixture"
	@echo "  pg-up       — start an ephemeral Postgres container for the test fixture"
	@echo "  pg-down     — stop + remove the test fixture container"
	@echo "  pg-logs     — tail the test fixture container's logs"
	@echo "  proto       — regenerate Go gRPC stubs from proto/loomcycle.proto"
	@echo "  proto-deps  — install the Go protoc plugins (one-time)"
	@echo ""
	@echo "Local DSN: $(PG_DSN)"

build:
	go build ./...

test:
	go test ./...

test-pg:
	@echo "Running tests with LOOMCYCLE_TEST_PG_DSN=$(PG_DSN)"
	LOOMCYCLE_TEST_PG_DSN="$(PG_DSN)" go test ./...

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
