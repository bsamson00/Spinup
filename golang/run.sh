#!/bin/bash
# Download, build and run the Go version of spinup.
#   curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/golang/run.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/golang/run.sh | bash -s -- --debug
# Installs the latest Go to /usr/local/go if Go isn't found. Builds as the
# current user; runs the binary with sudo (skipped for --debug or when already root).

# Wrapped in main() so bash reads the whole script before running it (curl | bash).
main() {
    set -euo pipefail

    local sudo=""
    [[ $EUID -ne 0 ]] && sudo="sudo"

    # Use Go from PATH, else an existing /usr/local/go, else install the latest release there.
    if ! command -v go >/dev/null 2>&1; then
        if [[ ! -x /usr/local/go/bin/go ]]; then
            local os arch ver
            os="$(uname -s | tr '[:upper:]' '[:lower:]')"
            case "$(uname -m)" in
                x86_64|amd64)  arch=amd64 ;;
                aarch64|arm64) arch=arm64 ;;
                *) echo "ERROR: unsupported architecture $(uname -m)" >&2; exit 1 ;;
            esac
            ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n1)"
            echo "Go not found; installing ${ver} (${os}-${arch}) to /usr/local/go ..."
            curl -fsSL "https://go.dev/dl/${ver}.${os}-${arch}.tar.gz" | $sudo tar -C /usr/local -xzf -
        fi
        export PATH="/usr/local/go/bin:$PATH"
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
