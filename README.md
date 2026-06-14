# vmsetup

Interactive first-boot provisioning script for fresh **Ubuntu 24.04 (noble)** and **26.04 (resolute)** servers/VMs. One script, a TUI to pick options, then it installs and hardens everything and reboots.

Personal use only.

## Quick start

Run as root on a fresh install:

```bash
sudo bash setup.sh
```

Or pipe it straight from the remote:

```bash
curl -fsSL https://gitea.lan2.me/Personal/vmsetup/raw/branch/main/setup.sh | sudo bash
```

The script reattaches stdin to the terminal, so `curl | bash` still gives you the interactive form.

## What it does

On launch it shows a **preflight TUI form** to configure the run, then executes the selected steps with a live progress screen, and finally **reboots after 10 seconds**.

### Preflight form options

**Account**
- Create a new user (username + password) **or** use the detected `sudo` user / an existing one
- GitHub username — pulls SSH public keys from `github.com/<user>.keys`. Leave blank to paste a key manually on a follow-up screen
- Hostname (blank = keep current)

**AI agents** (optional toggles)
- Claude Code (`claude.ai/install.sh`) — on by default
- OpenAI Codex (`npm i -g @openai/codex`)
- Google Antigravity / `agy` (`antigravity.google/cli/install.sh`)

**Infrastructure** (optional toggles)
- QEMU Guest Agent — off by default, and the toggle is **only shown when running on a QEMU/KVM guest** (e.g. a Proxmox VM), detected via `systemd-detect-virt`. On bare metal or other hypervisors it is hidden and cannot be installed.
- Tailscale (`tailscale.com/install.sh`)

### Steps performed

Always run:

| Group | Step | Detail |
|---|---|---|
| System base | System update & upgrade | `apt update && apt upgrade` |
| System base | Essential packages | build-essential, net-tools, btop, git, ca-certificates, gnupg, lsb-release |
| System base | Node.js 22.x | via NodeSource |
| System base | Docker Engine | official Docker repo + compose/buildx plugins |
| Account & access | User account | create or reuse; adds to `sudo` and `docker` groups |
| Account & access | SSH keys | from GitHub or manual paste → `authorized_keys` |
| Account & access | SSH hardening | disable root login, disable password auth, restart sshd |
| System config | Timezone | `America/New_York` |
| System config | Hostname | set if provided |
| System config | Speedtest CLI | via snap |

Conditional (per toggles): Claude Code, OpenAI Codex, Antigravity, QEMU Guest Agent, Tailscale.

Also done silently (not shown as a step): npm global prefix set to `~/.npm-global` so the user installs globals without `sudo`, and `~/.local/bin` / `~/.npm-global/bin` added to PATH in `.bashrc`.

## Result

After completion the script prints a summary (hostname, user, SSH mode, timezone, log path, `ssh user@ip` line) and reboots. End state:

- Non-root user with `sudo` + `docker`, key-only SSH
- Root SSH login disabled, password auth disabled
- Docker, Node 22, and selected agents/tools installed

## Notes & guards

- **Must be run as root.** Exits otherwise.
- **OS guard:** only noble/resolute pass; anything else exits.
- **No `set -e`** by design — the TUI uses `(( ))` arithmetic. Step failures are caught explicitly; a failed step stops the run and points you at the log.
- **Logging:** writes `ubuntu-setup-<timestamp>.log` (moved into the new user's home once the account exists).
- **TUI:** resize-aware (redraws on `SIGWINCH`); needs at least 54×18 terminal.
- **It reboots at the end.** Don't run it on a box you can't afford to bounce.
