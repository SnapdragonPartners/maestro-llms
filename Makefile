.PHONY: build test test-integration test-integration-local test-coverage lint fix fix-imports tidy install-lint install-goimports install-hooks clean

UNAME_S := $(shell uname -s)

# Default the local/CI Ollama model to a small non-reasoning model so the
# integration tests work out of the box. `?=` defers to an already-set env
# var (CI sets it explicitly), and is split from `export` for macOS's stock
# GNU Make 3.81.
OLLAMA_MODEL ?= llama3.2:1b
export OLLAMA_MODEL

# Build all packages.
build: lint
	go build ./...

# Run unit tests with coverage.
# Single test: make test TESTARGS='-run TestName ./llms/...'
test:
	go test -cover $(TESTARGS) ./...

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

# Auto-fix import grouping.
fix-imports: install-goimports
	goimports -w .

# Run all automatic fixes.
fix: fix-imports
	@echo "Automatic fixes applied"

# Tidy module dependencies.
tidy:
	go mod tidy

# Install git hooks (non-fatal on read-only checkouts / CI).
install-hooks:
	@if [ -d .git ] && [ -w .git/hooks ]; then \
		cp hooks/pre-push .git/hooks/pre-push && chmod +x .git/hooks/pre-push; \
		echo "Git hooks installed"; \
	fi

# Remove build and coverage artifacts.
clean:
	rm -rf bin/ coverage/
