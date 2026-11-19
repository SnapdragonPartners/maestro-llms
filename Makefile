.PHONY: build test test-mcp test-otel test-integration test-integration-local test-coverage lint fix fix-imports tidy install-lint install-goimports install-hooks clean

UNAME_S := $(shell uname -s)

# Default the local/CI Ollama model to a small non-reasoning model so the
# integration tests work out of the box. `?=` defers to an already-set env
# var (CI sets it explicitly), and is split from `export` for macOS's stock
# GNU Make 3.81.
OLLAMA_MODEL ?= llama3.2:1b
export OLLAMA_MODEL

# Nested modules built, linted and tested beside the root. mcp/ (ADR-0049)
# and otel/ (ADR-0051) are their own modules so the MCP SDK and OpenTelemetry
# stay out of the root module's graph; examples/* are handled by `tidy` and
# CI's "Build examples" step.
NESTED_MODULES := mcp otel

# Build all packages.
build: lint
	go build ./...
	@for dir in $(NESTED_MODULES); do echo "==> $$dir"; (cd "$$dir" && go build ./...) || exit 1; done

# Run unit tests with coverage, root module and nested modules.
# Single test: make test TESTARGS='-run TestName ./llms/...'
# TESTARGS applies to the root module only, and when it is set the nested
# modules are skipped; use `make test-mcp TESTARGS=...` / `make test-otel
# TESTARGS=...` for the nested modules.
test:
	go test -cover $(TESTARGS) ./...
ifeq ($(strip $(TESTARGS)),)
	@for dir in $(NESTED_MODULES); do echo "==> $$dir"; (cd "$$dir" && go test -cover ./...) || exit 1; done
endif

# Run the mcp module's unit tests (ADR-0049).
test-mcp:
	cd mcp && go test -cover $(TESTARGS) ./...

# Run the otel module's unit tests (ADR-0051). Its live export test is
# build-tagged and env-gated; see otel/README.md.
test-otel:
	cd otel && go test -cover $(TESTARGS) ./...

# Live integration tests against real provider APIs. Build-tagged so the
# default `test` target and CI stay network-free. Each test skips unless its
# provider key/host is present (ANTHROPIC_API_KEY/MAESTRO_ANTHROPIC_API_KEY,
# OPENAI_API_KEY, GEMINI_API_KEY, a reachable Ollama).
#
# OS-aware so there is one correct command everywhere: on macOS, plain `go
# test` binaries are unsigned and AMFI/Gatekeeper (often plus endpoint
# security) wedges them in dyld before any Go code runs — so we route to the
# ad-hoc-codesign script. Linux/CI keeps the plain `go test` path unchanged.
test-integration:
ifeq ($(UNAME_S),Darwin)
	@echo "==> macOS: routing through ad-hoc codesign (scripts/integration-local.sh)"
	./scripts/integration-local.sh
else
	go test -tags=integration -run Integration -count=1 -v ./llms/providers/...
endif

# Explicit escape hatch: force the codesign script regardless of OS. The
# script itself no-ops the signing on non-Darwin, so this is safe anywhere;
# `test-integration` already selects it automatically on macOS.
test-integration-local:
	./scripts/integration-local.sh

# Generate an HTML coverage report.
test-coverage:
	@mkdir -p coverage
	go test -coverprofile=coverage/coverage.out ./...
	go tool cover -html=coverage/coverage.out -o coverage/coverage.html
	@echo "Coverage report: coverage/coverage.html"

# Install golangci-lint if not present.
install-lint:
	@which golangci-lint > /dev/null || { \
		echo "Installing golangci-lint..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	}

# Install goimports if not present.
install-goimports:
	@which goimports > /dev/null || { \
		echo "Installing goimports..."; \
		go install golang.org/x/tools/cmd/goimports@latest; \
	}

# Run formatting and linting.
lint: install-lint
	go fmt ./...
	golangci-lint run
	@for dir in $(NESTED_MODULES); do echo "==> $$dir"; (cd "$$dir" && go fmt ./... && golangci-lint run) || exit 1; done

# Auto-fix import grouping.
fix-imports: install-goimports
	goimports -w .

# Run all automatic fixes.
fix: fix-imports
	@echo "Automatic fixes applied"

# Tidy module dependencies.
# Tidy the root module and every examples/* module. Examples `replace` onto
# the root, so any root dependency change must be followed by this (CI's
# "Build examples" step fails on drift).
tidy:
	go mod tidy
	@for dir in $(NESTED_MODULES); do echo "==> $$dir"; (cd "$$dir" && go mod tidy) || exit 1; done
	@for mod in examples/*/go.mod; do \
		dir=$$(dirname "$$mod"); echo "==> $$dir"; \
		(cd "$$dir" && go mod tidy) || exit 1; \
	done

# Install git hooks (non-fatal on read-only checkouts / CI).
install-hooks:
	@if [ -d .git ] && [ -w .git/hooks ]; then \
		cp hooks/pre-push .git/hooks/pre-push && chmod +x .git/hooks/pre-push; \
		echo "Git hooks installed"; \
	fi

# Remove build and coverage artifacts.
clean:
	rm -rf bin/ coverage/
