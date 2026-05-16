.PHONY: build test test-integration test-integration-local test-coverage lint fix fix-imports tidy install-lint install-goimports install-hooks clean

# Build all packages.
build: lint
	go build ./...

# Run unit tests with coverage.
# Single test: make test TESTARGS='-run TestName ./llms/...'
test:
	go test -cover $(TESTARGS) ./...

# Live integration tests against real provider APIs. Build-tagged so the
# default `test` target and CI stay network-free. Each test skips unless its
# provider key is set: ANTHROPIC_API_KEY, OPENAI_API_KEY.
test-integration:
	go test -tags=integration -run Integration -count=1 -v ./llms/providers/...

# Local macOS path: compile + ad-hoc codesign each integration test binary
# before running it. Plain `go test` binaries are unsigned and macOS
# (AMFI/Gatekeeper, often plus endpoint security) wedges them in dyld before
# any Go code runs. CI is Linux and uses `test-integration` directly.
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
