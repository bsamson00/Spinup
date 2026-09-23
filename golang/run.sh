#!/bin/bash
# Download, build and run the Go version of spinup.
#   curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/golang/run.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/golang/run.sh | bash -s -- --debug
# Requires Go 1.21+. Builds as the current user; runs the binary with sudo
# (skipped for --debug or when already root).

# Wrapped in main() so bash reads the whole script before running it (curl | bash).
main() {
    set -euo pipefail

    if ! command -v go >/dev/null 2>&1; then
        echo "ERROR: Go is not installed (need Go 1.21+)." >&2
        exit 1
    fi

    local dir
    dir="$(mktemp -d)"
    trap 'rm -rf "$dir"' EXIT

    # The binary goes in the current directory: spinup writes its log next to itself.
    curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/golang/spinup.go -o "$dir/spinup.go"
    go build -o ./spinup "$dir/spinup.go"

    # spinup reads keys from /dev/tty itself, so a piped stdin is fine.
    if [[ "${1:-}" == "--debug" || $EUID -eq 0 ]]; then
        ./spinup "$@"
    else
        sudo ./spinup "$@"
    fi
}

main "$@"
