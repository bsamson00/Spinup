# spinup

Interactive first-boot provisioning script for fresh **Ubuntu 24.04 (noble)** and **26.04 (resolute)** servers/VMs. One script, a TUI to pick options, then it installs and hardens everything and reboots.

> **A personal project.** I built spinup for my own servers and VMs so I could stop repeating the same setup and hardening steps by hand every time I spun up a new box. I'm sharing it in case it saves someone else that time too. It reflects my own preferences (tools, defaults, SSH policy), so read through it before running it and adapt it to your needs. It's provided as-is, with no warranty. See the [license](LICENSE).

## Quick start

Run as root on a fresh install:

```bash
sudo bash setup.sh
```

Or pipe it straight from the remote:

```bash
curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/setup.sh | sudo bash
```

The script reattaches stdin to the terminal, so `curl | bash` still gives you the interactive form.

### Go version

[`golang/spinup.go`](golang/spinup.go) is a single-file Go port with the same form, steps and guards. It uses only the standard library (Go 1.21+), so no `go.mod` is needed.

Build on any machine with Go, then copy the binary to the server and run it:

```bash
GOOS=linux GOARCH=amd64 go build -o spinup ./golang/spinup.go   # GOARCH=arm64 for ARM
scp spinup user@server:
ssh -t user@server 'sudo ./spinup'
```

Or run it straight from the remote on a server that already has Go installed:

```bash
curl -fsSL https://raw.githubusercontent.com/bsamson00/spinup/main/golang/spinup.go -o spinup.go
go build -o spinup spinup.go && sudo ./spinup
```

Build as your user and run the binary with `sudo`. `sudo go run` often fails because root's `PATH` doesn't include Go.

Add `--debug` to walk through the UI without changing anything. In debug mode the Go version also runs on macOS:

```bash
go run ./golang/spinup.go --debug
```

Differences from `setup.sh`: a step fails on its first failing command (not just its last), `curl | bash` installers run with `pipefail`, each command is logged as a `+ cmd` line, and the log is created next to the binary (falling back to `/tmp`).

## What it does

On launch it shows a **preflight TUI form** to configure the run, then a **review & confirm screen** summarizing everything it's about to do. Nothing is installed until you choose **Install** there. It then executes the selected steps with a live progress screen, and finally **reboots after 10 seconds**.

### Preflight form options

**Account**

- Create a new user (username + password) **or** use the detected `sudo` user / an existing one
- GitHub username — pulls SSH public keys from `github.com/<user>.keys`. The keys are fetched and validated when you submit the form; if none are valid the form won't continue. Leave blank to paste a key manually on a follow-up screen (also validated)
- Hostname (blank = keep current)
- Timezone — searchable picker (type to filter, ↑/↓, Enter). Defaults to `America/New_York`

**AI agents** (optional toggles)

- Claude Code (`claude.ai/install.sh`) — on by default
- OpenAI Codex (`npm i -g @openai/codex`)
- Google Antigravity / `agy` (`antigravity.google/cli/install.sh`)

**Infrastructure** (optional toggles)

- QEMU Guest Agent — off by default, and the toggle is **only shown when running on a QEMU/KVM guest** (e.g. a Proxmox VM), detected via `systemd-detect-virt`. On bare metal or other hypervisors it is hidden and cannot be installed.
- Tailscale (`tailscale.com/install.sh`) — installs only. After the reboot, run `sudo tailscale up` to authenticate and join your tailnet

**Controls:** Tab / ↑↓ move between fields, Space toggles an option or opens the timezone picker, and the **REVIEW & INSTALL** button at the bottom validates the form and continues.

### Review & confirm

Before anything runs, a summary screen lists:

- **Account & access** — the user (new, or existing with a note that its `authorized_keys` will be replaced), the SSH key source (GitHub user and key count, or the pasted key's type and fingerprint), and that root login and password auth will be disabled
- **System** — hostname, timezone, and the base packages that are always installed
- **Optional** — selected AI agents and infrastructure

Choose **Install** to start or **Back** (or Esc) to return to the form with your entries kept. **Back is selected by default**, so a stray Enter won't start the install. If you pasted an SSH key manually, you'll be asked for it again after going back.

### Steps performed

Always run:

| Group | Step | Detail |
|---|---|---|
| System base | System update & upgrade | `apt update && apt upgrade` |
| System base | Essential packages | build-essential, net-tools, btop, git, ca-certificates, gnupg, lsb-release |
| System base | Node.js (latest LTS) | via NodeSource |
| System base | Docker Engine | official Docker repo + compose/buildx plugins |
| Account & access | User account | create or reuse; adds to `sudo` and `docker` groups |
| Account & access | SSH keys | from GitHub or manual paste → `authorized_keys` (must contain a valid key) |
| Account & access | SSH hardening | disable root login, disable password auth, restart sshd (refuses if `authorized_keys` has no valid key) |
| System config | Timezone | selected in the form (default `America/New_York`) |
| System config | Hostname | set if provided |
| System config | Speedtest CLI | via snap |

Conditional (per toggles): Claude Code, OpenAI Codex, Antigravity, QEMU Guest Agent, Tailscale.

Also done silently (not shown as a step): npm global prefix set to `~/.npm-global` so the user installs globals without `sudo`, and `~/.local/bin` / `~/.npm-global/bin` added to PATH in `.bashrc`.

## Result

After completion the script prints a summary (hostname, user, SSH mode, timezone, log path, `ssh user@ip` line) and reboots. End state:

- Non-root user with `sudo` + `docker`, key-only SSH
- Root SSH login disabled, password auth disabled
- Docker, the latest Node.js LTS, and selected agents/tools installed
- Tailscale (if selected) installed but not connected — run `sudo tailscale up`

## Notes & guards

- **Must be run as root.** Exits otherwise.
- **OS guard:** only noble/resolute pass; anything else exits.
- **Input validation:** usernames (`a-z 0-9 _ -`, max 32, not `root`), existing usernames (must exist), GitHub usernames, and hostnames (RFC 1123) are checked in the form, before any step runs.
- **Lockout guard:** password SSH login is only disabled after a valid public key is installed.
- **Home directory:** resolved from the account (`getent passwd`), so existing users with a non-`/home` home work.
- **No `set -e`** by design — the TUI uses `(( ))` arithmetic. Step failures are caught explicitly; a failed step stops the run and points you at the log.
- **Logging:** writes `ubuntu-setup-<timestamp>.log` (moved into the new user's home once the account exists).
- **TUI:** resize-aware (redraws on `SIGWINCH`); needs at least 54×18 terminal.
- **It reboots at the end.** Don't run it on a box you can't afford to bounce.

## License

MIT — see [LICENSE](LICENSE).
