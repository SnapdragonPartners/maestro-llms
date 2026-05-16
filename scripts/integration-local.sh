#!/usr/bin/env bash
# Run the //go:build integration provider tests locally on macOS.
#
# Why this exists: macOS (AMFI/Gatekeeper, often plus endpoint-security
# agents) blocks execution of freshly built *unsigned* binaries — a `go test`
# run wedges in dyld at `_dyld_start` before any Go code runs (no output, no
# test timeout, no panic). Ad-hoc code-signing the compiled test binary
# satisfies the gate (same technique as maestro's run.sh). CI runs Linux and
# is unaffected; this is the local-dev convenience path.
#
# Each provider test skips unless its credentials/host are present, so a
# missing key degrades to a skip, not a failure. Environment is passed
# through; set keys/OLLAMA_MODEL as needed, e.g.:
#
#   OPENAI_API_KEY=... OLLAMA_MODEL=llama3.2:1b scripts/integration-local.sh
#
# Optional: INTEGRATION_TIMEOUT (default 300s), and extra `go test` args via
# positional arguments (e.g. -test.run TestIntegrationOllama).

set -uo pipefail

cd "$(dirname "$0")/.."
root="$(pwd)"
timeout="${INTEGRATION_TIMEOUT:-300s}"
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT

# Discover provider packages that actually have integration tests.
pkgs=()
while IFS= read -r f; do
	pkgs+=("$(dirname "$f")")
done < <(find llms/providers -name integration_test.go | sort)

if [ "${#pkgs[@]}" -eq 0 ]; then
	echo "no integration_test.go files found" >&2
	exit 1
fi

overall=0
for pkg in "${pkgs[@]}"; do
	name="$(basename "$pkg")"
	bin="$staging/${name}.test"
	echo "==> building ${pkg} (integration)"
	if ! go test -c -tags=integration -o "$bin" "./${pkg}/"; then
		echo "BUILD FAILED: ${pkg}" >&2
		overall=1
		continue
	fi
	# Strip quarantine/extended attributes, then ad-hoc sign so macOS will
	# actually exec the fresh binary.
	xattr -c "$bin" 2>/dev/null || true
	codesign --force --sign - --timestamp=none "$bin"

	echo "==> running ${name} integration tests"
	if ! "$bin" -test.run Integration -test.v -test.timeout="$timeout" "$@"; then
		overall=1
	fi
done

if [ "$overall" -eq 0 ]; then
	echo "integration-local: PASS"
else
	echo "integration-local: FAILURES (see above)" >&2
fi
exit "$overall"
