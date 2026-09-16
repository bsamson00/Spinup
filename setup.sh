#!/bin/bash

# Wrapping in main() forces bash to read the entire script from the pipe
# before executing anything. This is required for curl | bash to work
# with interactive input.
main() {

# If stdin is a pipe (curl | bash), redirect it to the terminal
if [ ! -t 0 ]; then
    exec 0</dev/tty
fi

# NOTE: 'set -e' is intentionally NOT used. The TUI relies heavily on
# (( ... )) arithmetic, which returns non-zero on a zero result and would
# abort under -e. Step failures are handled explicitly in run_step().

# ============================================================
# Ubuntu 24.04 / 26.04 Fresh Install Setup Script
# Must be run as root (sudo)
# ============================================================

# --debug: full UI walkthrough, but no step runs, nothing is changed, no reboot
DEBUG=0
[[ "${1:-}" == "--debug" ]] && DEBUG=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd 2>/dev/null || echo /tmp)"
LOG_FILE="${SCRIPT_DIR}/ubuntu-setup-$(date +%Y%m%d-%H%M%S).log"

MARGIN=2
INDENT=4
LIST_START=7

# ============================================================
# COLORS AND SYMBOLS
# ============================================================
RESET="\033[0m"
BOLD="\033[1m"
DIM="\033[2m"

RED="\033[31m"
GREEN="\033[32m"
YELLOW="\033[33m"
CYAN="\033[36m"
WHITE="\033[37m"
BLACK="\033[90m"

BR_RED="\033[91m"
BR_GREEN="\033[92m"
BR_YELLOW="\033[93m"
BR_BLUE="\033[94m"
BR_CYAN="\033[96m"
BR_WHITE="\033[97m"

CHECK="✔"
CROSS="✖"
ARROW="▸"
BAR="▌"
DOT_ON="◉"
DOT_OFF="○"
SKIP_GLYPH="◇"
RUN_GLYPH="◌"
BLOCK_FULL="█"
BLOCK_LIGHT="░"
SPINNER_CHARS=("⠋" "⠙" "⠹" "⠸" "⠼" "⠴" "⠦" "⠧" "⠇" "⠏")

# ============================================================
# OS DETECTION & COMPATIBILITY GUARD
# Supports Ubuntu 24.04 (noble) and 26.04 (resolute) only.
# ============================================================
if [ -r /etc/os-release ]; then
    . /etc/os-release
else
    printf "${BR_RED}${BOLD}ERROR:${RESET} Unsupported system.\n"
    exit 1
fi

UBUNTU_VERSION_ID="${VERSION_ID:-unknown}"
UBUNTU_CODENAME_DETECTED="${UBUNTU_CODENAME:-${VERSION_CODENAME:-unknown}}"
SETUP_BOX_TITLE="UBUNTU ${UBUNTU_VERSION_ID} SERVER SETUP"
(( DEBUG )) && SETUP_BOX_TITLE+=" [DEBUG]"

case "$UBUNTU_CODENAME_DETECTED" in
    noble|resolute) ;;
    *)
        if (( ! DEBUG )); then
            printf "${BR_RED}${BOLD}ERROR:${RESET} This script supports Ubuntu 24.04 (noble) and 26.04 (resolute) only.\n"
            printf "${DIM}Detected: ${PRETTY_NAME:-$UBUNTU_VERSION_ID} (codename: ${UBUNTU_CODENAME_DETECTED})${RESET}\n"
            exit 1
        fi
        ;;
esac

# ============================================================
# VIRTUALIZATION DETECTION
# QEMU Guest Agent is only relevant on a QEMU/KVM guest (which is
# what a Proxmox VM is). Gate the option on detecting that host type;
# on bare metal or other hypervisors the toggle is hidden entirely.
# ============================================================
IS_QEMU_GUEST=0
case "$(systemd-detect-virt 2>/dev/null)" in
    qemu|kvm) IS_QEMU_GUEST=1 ;;
esac

# ============================================================
# ROOT CHECK
# ============================================================
if [[ $EUID -ne 0 ]] && (( ! DEBUG )); then
    printf "${BR_RED}${BOLD}ERROR:${RESET} This script must be run as root (sudo).\n"
    exit 1
fi

# ============================================================
# NON-INTERACTIVE APT / NEEDRESTART
# Exported globally so every step (including the node/docker/tailscale
# installer scripts that run their own apt) inherits these. needrestart
# would otherwise pop a "Pending kernel upgrade" / service-restart dialog
# that blocks apt; suspend it entirely since we reboot at the end.
# ============================================================
export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
export NEEDRESTART_SUSPEND=1

# ============================================================
# LOGGING
# ============================================================
touch "$LOG_FILE"
echo "=== Ubuntu ${UBUNTU_VERSION_ID} Setup Started: $(date) ===" >> "$LOG_FILE"
log() { echo "=== [$(date)] $* ===" >> "$LOG_FILE"; }

# ============================================================
# TERMINAL HELPERS (resize-aware)
# ============================================================
TERM_COLS=80
TERM_LINES=24

update_dims() {
    TERM_COLS=$(tput cols 2>/dev/null || echo 80)
    TERM_LINES=$(tput lines 2>/dev/null || echo 24)
}
update_dims

hide_cursor() { printf "\033[?25l"; }
show_cursor() { printf "\033[?25h"; }
move_to()     { printf "\033[%d;%dH" "$1" "$2"; }
clear_line()  { printf "\033[2K"; }
clear_screen(){ printf "\033[2J\033[H"; }

# Horizontal rule (prints n copies of a char)
hr() { local ch="$1" n="$2" i; (( n < 1 )) && return; for ((i=0; i<n; i++)); do printf "%s" "$ch"; done; }
# Returns a repeated-char string (for value rendering)
hr_str() { local ch="$1" n="$2" s="" i; for ((i=0; i<n; i++)); do s+="$ch"; done; printf "%s" "$s"; }

# Track active screen so SIGWINCH can redraw the right thing
CURRENT_SCREEN=""
on_winch() {
    update_dims
    case "$CURRENT_SCREEN" in
        form)     render_form ;;
        progress)
            stop_spinner
            render_progress
            [[ -n "$RUNNING_IDX" ]] && start_spinner "$RUNNING_IDX"
            ;;
        sshpaste) render_sshpaste ;;
        tzpick)   render_tzpick ;;
    esac
}
trap on_winch WINCH
trap 'show_cursor; stty echo 2>/dev/null; exit' EXIT INT TERM

too_small() {
    clear_screen
    move_to 1 1
    printf "${BR_YELLOW}Terminal too small.${RESET}\n"
    printf "${DIM}Please enlarge the window (need at least 54 cols x 18 lines).${RESET}\n"
}

# ============================================================
# SHARED DRAWING
# ============================================================
draw_box() {
    local title="$1" color="$2" inner=$((TERM_COLS - 2))
    printf "${color}${BOLD}╔"; hr "═" "$inner"; printf "╗${RESET}\n"
    local pad=$(( (inner - ${#title}) / 2 )); (( pad < 0 )) && pad=0
    local rpad=$(( inner - pad - ${#title} )); (( rpad < 0 )) && rpad=0
    printf "${color}${BOLD}║${RESET}"; hr " " "$pad"
    printf "${BOLD}${BR_WHITE}%s${RESET}" "$title"; hr " " "$rpad"
    printf "${color}${BOLD}║${RESET}\n"
    printf "${color}${BOLD}╚"; hr "═" "$inner"; printf "╝${RESET}\n"
}

draw_bar() {
    local percent=$1
    local bar_width=$((TERM_COLS - 20)); (( bar_width < 10 )) && bar_width=10
    local filled=$((percent * bar_width / 100))
    local empty=$((bar_width - filled))
    move_to 5 1; clear_line
    printf "  ${BOLD}${BR_WHITE}%3d%%${RESET} ${DIM}${CYAN}│${RESET}" "$percent"
    local i
    for ((i=0; i<filled; i++)); do
        if   (( i < bar_width / 3 ));     then printf "${BR_BLUE}${BLOCK_FULL}${RESET}"
        elif (( i < bar_width * 2 / 3 )); then printf "${BR_CYAN}${BLOCK_FULL}${RESET}"
        else printf "${BR_GREEN}${BLOCK_FULL}${RESET}"; fi
    done
    for ((i=0; i<empty; i++)); do printf "${DIM}${BLACK}${BLOCK_LIGHT}${RESET}"; done
    printf "${DIM}${CYAN}│${RESET}"
}

# Format a duration in seconds as M:SS / H:MM:SS
fmt_elapsed() {
    local e="$1"; (( e < 0 )) && e=0
    local h=$(( e / 3600 )) m=$(( (e % 3600) / 60 )) s=$(( e % 60 ))
    if (( h > 0 )); then printf "%d:%02d:%02d" "$h" "$m" "$s"; else printf "%d:%02d" "$m" "$s"; fi
}

# Live elapsed-time clock, drawn at the top-right of the box title row
draw_timer() {
    (( START_EPOCH == 0 )) && return
    local now; printf -v now '%(%s)T' -1
    local label; label="$(fmt_elapsed $(( now - START_EPOCH )))"
    local col=$(( TERM_COLS - ${#label} - 4 )); (( col < 1 )) && col=1
    move_to 2 "$col"; printf "${BR_CYAN}${BOLD}⏱ %s${RESET}" "$label"
}

# ============================================================
# PREFLIGHT FORM MODEL (single consolidated screen)
# ============================================================
FORM_KIND=()
FORM_KEY=()
FORM_LABEL=()
declare -A FORM_KIND_OF
declare -A FV
declare -A ROW_OF VALCOL_OF VALW_OF

ACTIVE=0
FORM_ERROR=""
FORM_STATUS=""
DETECTED_USER=""
GITHUB_KEYS=""
DEFAULT_TIMEZONE="America/New_York"

# ============================================================
# INPUT VALIDATION
# ============================================================
valid_username() {
    local re='^[a-z_][a-z0-9_-]{0,31}$'
    [[ "$1" =~ $re && "$1" != "root" ]]
}
valid_github_user() {
    local re='^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$'
    [[ "$1" =~ $re ]]
}
valid_hostname() {
    local h="$1" label parts re='^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$'
    (( ${#h} <= 253 )) || return 1
    [[ "$h" == .* || "$h" == *. ]] && return 1
    IFS=. read -ra parts <<< "$h"
    for label in "${parts[@]}"; do [[ "$label" =~ $re ]] || return 1; done
    return 0
}
# True if the text contains at least one parseable SSH public key
valid_ssh_keys() {
    [[ -n "$1" ]] && ssh-keygen -lf /dev/stdin <<< "$1" >/dev/null 2>&1
}

add_item() {
    FORM_KIND+=("$1"); FORM_KEY+=("$2"); FORM_LABEL+=("$3")
    FORM_KIND_OF["$2"]="$1"
}

build_form() {
    add_item group  G_ACCOUNT    "ACCOUNT"
    add_item toggle CREATE_USER  "Create a new user account"
    add_item note   USERNOTE     "Using existing user"
    add_item text   USERNAME     "Username"
    add_item secret PASSWORD     "Password"
    add_item text   GITHUB       "GitHub user (SSH keys)"
    add_item text   HOSTNAME     "Hostname (blank = keep)"
    add_item select TIMEZONE     "Timezone"
    add_item group  G_AGENTS     "AI AGENTS"
    add_item toggle AGENT_CLAUDE "Claude Code"
    add_item toggle AGENT_CODEX  "OpenAI Codex"
    add_item toggle AGENT_AGY    "Google Antigravity (agy)"
    add_item group  G_INFRA      "INFRASTRUCTURE"
    add_item toggle QEMU         "QEMU Guest Agent"
    add_item toggle TAILSCALE    "Tailscale"

    FV[USERNAME]=""; FV[PASSWORD]=""; FV[GITHUB]=""; FV[HOSTNAME]=""
    FV[TIMEZONE]="$DEFAULT_TIMEZONE"
    FV[AGENT_CLAUDE]=1; FV[AGENT_CODEX]=0; FV[AGENT_AGY]=0
    FV[QEMU]=0; FV[TAILSCALE]=0
    if [[ -n "$DETECTED_USER" ]]; then FV[CREATE_USER]=0; else FV[CREATE_USER]=1; fi
}

field_visible() {
    case "$1" in
        USERNAME)
            [[ "${FV[CREATE_USER]}" == "1" ]] && return 0
            [[ -z "$DETECTED_USER" ]] && return 0 || return 1 ;;
        PASSWORD)
            [[ "${FV[CREATE_USER]}" == "1" ]] && return 0 || return 1 ;;
        USERNOTE)
            [[ "${FV[CREATE_USER]}" != "1" && -n "$DETECTED_USER" ]] && return 0 || return 1 ;;
        QEMU)
            (( IS_QEMU_GUEST )) && return 0 || return 1 ;;
        *) return 0 ;;
    esac
}

is_focusable_idx() {
    local i="$1"
    local k="${FORM_KIND[$i]}" key="${FORM_KEY[$i]}"
    [[ "$k" == "group" || "$k" == "note" ]] && return 1
    field_visible "$key" || return 1
    return 0
}

focus_next() {
    local n=${#FORM_KIND[@]} j=$ACTIVE c=0
    while (( c < n )); do j=$(( (j + 1) % n )); ((c++)); if is_focusable_idx "$j"; then ACTIVE=$j; return; fi; done
}
focus_prev() {
    local n=${#FORM_KIND[@]} j=$ACTIVE c=0
    while (( c < n )); do j=$(( (j - 1 + n) % n )); ((c++)); if is_focusable_idx "$j"; then ACTIVE=$j; return; fi; done
}

draw_form_value() {
    local key="$1"
    local valcol=${VALCOL_OF[$key]} valw=${VALW_OF[$key]} row=${ROW_OF[$key]}
    [[ -z "$valcol" ]] && return
    local val="${FV[$key]}" disp
    if [[ "${FORM_KIND_OF[$key]}" == "secret" ]]; then
        local n=${#val}; (( n > valw )) && n=$valw
        disp=$(hr_str "$DOT_ON" "$n")
    else
        disp="$val"
        (( ${#disp} > valw )) && disp="${disp: -valw}"
    fi
    move_to "$row" $((valcol + 2)); printf "%-*s" "$valw" "$disp"
}

draw_form_field() {
    local i="$1" row="$2"
    local kind="${FORM_KIND[$i]}" key="${FORM_KEY[$i]}" label="${FORM_LABEL[$i]}"
    local active=0; [[ "$i" == "$ACTIVE" ]] && active=1
    move_to "$row" 1; clear_line

    local marker="  "
    (( active )) && marker="${BR_CYAN}${BOLD}${ARROW} ${RESET}"

    if [[ "$kind" == "toggle" ]]; then
        local box="${DIM}${DOT_OFF}${RESET}"
        [[ "${FV[$key]}" == "1" ]] && box="${BR_GREEN}${DOT_ON}${RESET}"
        if (( active )); then
            printf "    ${marker}${box} ${BOLD}${BR_WHITE}%s${RESET}" "$label"
        else
            printf "    ${marker}${box} ${WHITE}%s${RESET}" "$label"
        fi
        return
    fi

    # text / secret / select
    local lbl="$label"
    [[ "$key" == "USERNAME" && "${FV[CREATE_USER]}" != "1" ]] && lbl="Existing username"
    local labelw=22; (( TERM_COLS < 72 )) && labelw=15
    if (( active )); then printf "    ${marker}${BOLD}${BR_WHITE}%-*s${RESET}" "$labelw" "$lbl"
    else printf "    ${marker}${DIM}%-*s${RESET}" "$labelw" "$lbl"; fi

    local valcol=$((INDENT + 4 + labelw + 1))
    local valw=$((TERM_COLS - valcol - 4)); (( valw > 40 )) && valw=40; (( valw < 8 )) && valw=8
    VALCOL_OF[$key]=$valcol; VALW_OF[$key]=$valw
    local bc="${DIM}${CYAN}"; (( active )) && bc="${BR_CYAN}"
    move_to "$row" "$valcol"; printf "${bc}[ ${RESET}"
    draw_form_value "$key"
    move_to "$row" $((valcol + 2 + valw + 1)); printf "${bc} ]${RESET}"
}

place_form_cursor() {
    local key="${FORM_KEY[$ACTIVE]}" kind="${FORM_KIND[$ACTIVE]}"
    if [[ "$kind" == "text" || "$kind" == "secret" ]]; then
        local valcol=${VALCOL_OF[$key]} valw=${VALW_OF[$key]} val="${FV[$key]}"
        local len=${#val}; (( len > valw )) && len=$valw
        show_cursor; move_to "${ROW_OF[$key]}" $((valcol + 2 + len))
    else
        hide_cursor
    fi
}

render_form() {
    if (( TERM_COLS < 54 || TERM_LINES < 18 )); then too_small; return; fi
    clear_screen
    draw_box "$SETUP_BOX_TITLE" "$BR_CYAN"
    local row=5
    move_to "$row" 1; printf "  ${BOLD}${BR_WHITE}Preflight Configuration${RESET}"; ((row++))
    move_to "$row" 1; printf "  ${DIM}${CYAN}"; hr "─" $((TERM_COLS - 4)); printf "${RESET}"; ((row += 2))

    local i
    for i in "${!FORM_KIND[@]}"; do
        local kind="${FORM_KIND[$i]}" key="${FORM_KEY[$i]}" label="${FORM_LABEL[$i]}"
        if [[ "$kind" == "group" ]]; then
            ((row++))
            move_to "$row" 1; printf "  ${BR_CYAN}${BOLD}${BAR} %s${RESET}" "$label"; ((row++))
            continue
        fi
        if [[ "$kind" == "note" ]]; then
            field_visible "$key" || continue
            move_to "$row" 1; printf "        ${DIM}%s: ${WHITE}%s${RESET}" "$label" "$DETECTED_USER"; ((row++))
            continue
        fi
        field_visible "$key" || continue
        ROW_OF[$key]=$row
        draw_form_field "$i" "$row"
        ((row++))
    done

    ((row++))
    move_to "$row" 1; printf "  ${DIM}Tab / ${ARROW}${ARROW} navigate    Space toggle / choose    Enter continue${RESET}"; ((row++))
    if [[ -n "$FORM_ERROR" ]]; then
        move_to "$row" 1; clear_line; printf "  ${BR_RED}%s${RESET}" "$FORM_ERROR"
    elif [[ -n "$FORM_STATUS" ]]; then
        move_to "$row" 1; clear_line; printf "  ${BR_YELLOW}%s${RESET}" "$FORM_STATUS"
    fi
    place_form_cursor
}

append_char() { local key="$1" c="$2"; (( ${#FV[$key]} < 64 )) && FV[$key]+="$c"; }

validate_form() {
    FORM_ERROR=""
    local u="${FV[USERNAME]}" gh="${FV[GITHUB]}" hn="${FV[HOSTNAME]}"
    if [[ "${FV[CREATE_USER]}" == "1" ]]; then
        [[ -z "$u" ]] && { FORM_ERROR="Username is required."; return 1; }
        valid_username "$u" || { FORM_ERROR="Invalid username: a-z 0-9 _ - only, max 32, not root."; return 1; }
        [[ -z "${FV[PASSWORD]}" ]] && { FORM_ERROR="Password is required."; return 1; }
    elif [[ -z "$DETECTED_USER" ]]; then
        [[ -z "$u" ]] && { FORM_ERROR="Enter an existing username, or enable 'Create a new user'."; return 1; }
        [[ "$u" == "root" ]] && { FORM_ERROR="Pick a non-root user."; return 1; }
        id "$u" &>/dev/null || { FORM_ERROR="User '$u' does not exist."; return 1; }
    fi
    [[ -n "$gh" ]] && ! valid_github_user "$gh" && { FORM_ERROR="Invalid GitHub username."; return 1; }
    [[ -n "$hn" ]] && ! valid_hostname "$hn" && { FORM_ERROR="Invalid hostname: letters, digits, - and . only."; return 1; }
    [[ -z "${FV[TIMEZONE]}" ]] && { FORM_ERROR="Timezone is required."; return 1; }

    # Fetch GitHub keys now so a user with no keys is caught before
    # anything runs (SSH hardening disables password login).
    GITHUB_KEYS=""
    if [[ -n "$gh" ]]; then
        FORM_STATUS="Fetching SSH keys from github.com/${gh}.keys ..."
        render_form
        FORM_STATUS=""
        GITHUB_KEYS="$(curl -fsSL --max-time 15 "https://github.com/${gh}.keys" 2>/dev/null)"
        if ! valid_ssh_keys "$GITHUB_KEYS"; then
            GITHUB_KEYS=""
            FORM_ERROR="No valid SSH keys at github.com/${gh}.keys (clear field to paste one)."
            return 1
        fi
    fi
    return 0
}

# ============================================================
# TIMEZONE PICKER (type-to-filter list)
# ============================================================
TZ_LIST=(); TZ_MATCH=(); TZ_FILTER=""; TZ_SEL=0; TZ_TOP=0

load_timezones() {
    (( ${#TZ_LIST[@]} )) && return
    mapfile -t TZ_LIST < <(timedatectl list-timezones 2>/dev/null)
    (( ${#TZ_LIST[@]} )) || TZ_LIST=("$DEFAULT_TIMEZONE" "UTC")
}

tz_apply_filter() {
    local z f="${TZ_FILTER,,}"
    TZ_MATCH=()
    for z in "${TZ_LIST[@]}"; do
        [[ -z "$f" || "${z,,}" == *"$f"* ]] && TZ_MATCH+=("$z")
    done
    TZ_SEL=0; TZ_TOP=0
}

render_tzpick() {
    if (( TERM_COLS < 54 || TERM_LINES < 18 )); then too_small; return; fi
    clear_screen
    draw_box "$SETUP_BOX_TITLE" "$BR_CYAN"
    move_to 5 1; printf "  ${BOLD}${BR_WHITE}Select Timezone${RESET}"
    move_to 6 1; printf "  ${DIM}${CYAN}"; hr "─" $((TERM_COLS - 4)); printf "${RESET}"
    move_to 8 1; printf "    ${DIM}Filter${RESET}  ${BR_CYAN}[ ${RESET}%s${BR_CYAN} ]${RESET}" "$TZ_FILTER"

    local n=${#TZ_MATCH[@]} row=10 visible=$(( TERM_LINES - 12 )) i
    (( visible < 1 )) && visible=1
    (( TZ_SEL < TZ_TOP )) && TZ_TOP=$TZ_SEL
    (( TZ_SEL >= TZ_TOP + visible )) && TZ_TOP=$(( TZ_SEL - visible + 1 ))
    if (( n == 0 )); then
        move_to "$row" 1; printf "      ${DIM}No matching timezones${RESET}"
    fi
    for (( i = TZ_TOP; i < n && i < TZ_TOP + visible; i++ )); do
        move_to "$row" 1
        if (( i == TZ_SEL )); then
            printf "    ${BR_CYAN}${BOLD}${ARROW} ${BR_WHITE}%s${RESET}" "${TZ_MATCH[$i]}"
        else
            printf "      ${WHITE}%s${RESET}" "${TZ_MATCH[$i]}"
        fi
        ((row++))
    done
    move_to $((TERM_LINES - 1)) 1
    printf "  ${DIM}Type to filter    Up/Down move    Enter select    Esc cancel    (%d/%d)${RESET}" "$n" "${#TZ_LIST[@]}"
}

run_tzpick() {
    load_timezones
    CURRENT_SCREEN="tzpick"
    hide_cursor
    TZ_FILTER=""; tz_apply_filter
    local i
    for i in "${!TZ_MATCH[@]}"; do [[ "${TZ_MATCH[$i]}" == "${FV[TIMEZONE]}" ]] && { TZ_SEL=$i; break; }; done
    render_tzpick
    local ch a b c page=$(( TERM_LINES - 12 ))
    (( page < 1 )) && page=1
    while true; do
        IFS= read -rsn1 ch || continue   # read interrupted (likely SIGWINCH); trap already redrew
        if [[ "$ch" == "" ]]; then                           # Enter
            (( ${#TZ_MATCH[@]} )) && { FV[TIMEZONE]="${TZ_MATCH[$TZ_SEL]}"; break; }
        elif [[ "$ch" == $'\x1b' ]]; then
            a=""; b=""
            IFS= read -rsn1 -t 0.05 a || true
            IFS= read -rsn1 -t 0.05 b || true
            [[ -z "$a" ]] && break                           # bare Esc = cancel
            if [[ "$a" == "[" ]]; then
                case "$b" in
                    A) (( TZ_SEL > 0 )) && ((TZ_SEL--)) ;;
                    B) (( TZ_SEL < ${#TZ_MATCH[@]} - 1 )) && ((TZ_SEL++)) ;;
                    5) IFS= read -rsn1 -t 0.05 c; TZ_SEL=$(( TZ_SEL - page < 0 ? 0 : TZ_SEL - page )) ;;
                    6) IFS= read -rsn1 -t 0.05 c; TZ_SEL=$(( TZ_SEL + page )); (( TZ_SEL > ${#TZ_MATCH[@]} - 1 )) && TZ_SEL=$(( ${#TZ_MATCH[@]} - 1 )); (( TZ_SEL < 0 )) && TZ_SEL=0 ;;
                esac
            fi
        elif [[ "$ch" == $'\x7f' || "$ch" == $'\x08' ]]; then
            TZ_FILTER="${TZ_FILTER%?}"; tz_apply_filter
        elif [[ "$ch" =~ [[:print:]] ]]; then
            (( ${#TZ_FILTER} < 40 )) && { TZ_FILTER+="$ch"; tz_apply_filter; }
        fi
        render_tzpick
    done
    CURRENT_SCREEN="form"
    render_form
}

run_form() {
    CURRENT_SCREEN="form"
    ACTIVE=0
    is_focusable_idx 0 || focus_next
    render_form
    local ch a b
    while true; do
        if IFS= read -rsn1 ch; then
            if [[ "$ch" == "" ]]; then                       # Enter
                local kind="${FORM_KIND[$ACTIVE]}"
                if [[ "$kind" == "text" || "$kind" == "secret" ]]; then
                    focus_next; render_form
                elif [[ "$kind" == "select" ]]; then
                    run_tzpick
                elif validate_form; then return
                else render_form; fi
            elif [[ "$ch" == $'\t' ]]; then
                focus_next; render_form
            elif [[ "$ch" == $'\x1b' ]]; then                # arrow keys
                IFS= read -rsn1 -t 0.05 a || true
                IFS= read -rsn1 -t 0.05 b || true
                if [[ "$a" == "[" ]]; then
                    case "$b" in
                        A) focus_prev ;;
                        B) focus_next ;;
                        Z) focus_prev ;;
                    esac
                    render_form
                fi
            elif [[ "$ch" == " " ]]; then
                local key="${FORM_KEY[$ACTIVE]}" kind="${FORM_KIND[$ACTIVE]}"
                if [[ "$kind" == "toggle" ]]; then
                    [[ "${FV[$key]}" == "1" ]] && FV[$key]=0 || FV[$key]=1
                    render_form
                elif [[ "$kind" == "select" ]]; then
                    run_tzpick
                else
                    append_char "$key" " "; draw_form_value "$key"; place_form_cursor
                fi
            elif [[ "$ch" == $'\x7f' || "$ch" == $'\x08' ]]; then
                local key="${FORM_KEY[$ACTIVE]}" kind="${FORM_KIND[$ACTIVE]}"
                if [[ "$kind" == "text" || "$kind" == "secret" ]]; then
                    FV[$key]="${FV[$key]%?}"; draw_form_value "$key"; place_form_cursor
                fi
            elif [[ "$ch" =~ [[:print:]] ]]; then
                local key="${FORM_KEY[$ACTIVE]}" kind="${FORM_KIND[$ACTIVE]}"
                if [[ "$kind" == "text" || "$kind" == "secret" ]]; then
                    append_char "$key" "$ch"; draw_form_value "$key"; place_form_cursor
                fi
            fi
        else
            continue   # read interrupted (likely SIGWINCH); trap already redrew
        fi
    done
}

# ============================================================
# SSH MANUAL PASTE SCREEN
# ============================================================
render_sshpaste() {
    clear_screen
    draw_box "$SETUP_BOX_TITLE" "$BR_CYAN"
    move_to 6 1
    printf "\n  ${BOLD}${BR_WHITE}SSH Public Key${RESET}\n"
    printf "  ${DIM}${CYAN}"; hr "─" $((TERM_COLS - 4)); printf "${RESET}\n\n"
    printf "  ${DIM}No GitHub username provided. Paste your SSH public key below.${RESET}\n"
    printf "  ${DIM}(Typically starts with ssh-rsa, ssh-ed25519, or ecdsa-sha2)${RESET}\n\n"
    [[ -n "$SSH_PASTE_ERROR" ]] && printf "  ${BR_RED}%s${RESET}\n\n" "$SSH_PASTE_ERROR"
    printf "  ${BR_CYAN}${ARROW}${RESET} "
    show_cursor
}

# ============================================================
# STEP REGISTRY (built from selections) + grouped progress
# ============================================================
R_GROUP=(); R_NAME=(); R_KEY=(); R_STATUS=()
declare -A RIDX
DISPLAY_KIND=(); DISPLAY_TEXT=(); DISPLAY_STEPIDX=()
declare -A STEP_DISP_LINE
DISPLAY_LEN=0
STATUS_SEP_ROW=0
STATUS_ROW=0
TOTAL_STEPS=0
CURRENT_STEP=0
LAST_STATUS="Starting..."
RUNNING_IDX=""
START_EPOCH=0

add_reg() { R_GROUP+=("$1"); R_NAME+=("$2"); R_KEY+=("$3"); R_STATUS+=("pending"); }

build_registry() {
    local user_desc ssh_desc
    if (( SKIP_USER_CREATION )); then user_desc="${SETUP_USERNAME}, existing"; else user_desc="${SETUP_USERNAME}"; fi
    if [[ "$SSH_KEY_SOURCE" == "github" ]]; then ssh_desc="github.com/${GITHUB_USER}"; else ssh_desc="manual key"; fi

    add_reg "SYSTEM BASE"      "System Update & Upgrade"   UPDATE
    add_reg "SYSTEM BASE"      "Essential Packages"        ESSENTIALS
    add_reg "SYSTEM BASE"      "Node.js 22.x"              NODE
    add_reg "SYSTEM BASE"      "Docker Engine"             DOCKER

    add_reg "ACCOUNT & ACCESS" "User Account (${user_desc})" USER
    add_reg "ACCOUNT & ACCESS" "SSH Keys (${ssh_desc})"      SSHKEYS
    add_reg "ACCOUNT & ACCESS" "SSH Hardening"               SSHHARDEN

    [[ "${FV[AGENT_CLAUDE]}" == "1" ]] && add_reg "AI AGENTS" "Claude Code"               CLAUDE
    [[ "${FV[AGENT_CODEX]}"  == "1" ]] && add_reg "AI AGENTS" "OpenAI Codex"              CODEX
    [[ "${FV[AGENT_AGY]}"    == "1" ]] && add_reg "AI AGENTS" "Google Antigravity (agy)"  AGY

    add_reg "SYSTEM CONFIG"    "Timezone: ${SETUP_TIMEZONE}" TZ
    add_reg "SYSTEM CONFIG"    "Hostname Configuration"     HOSTNAME
    add_reg "SYSTEM CONFIG"    "Speedtest CLI"              SPEEDTEST

    [[ "${FV[QEMU]}" == "1" ]] && (( IS_QEMU_GUEST )) && add_reg "INFRASTRUCTURE" "QEMU Guest Agent" QEMU
    [[ "${FV[TAILSCALE]}" == "1" ]] && add_reg "INFRASTRUCTURE" "Tailscale"        TAILSCALE

    local i
    for i in "${!R_KEY[@]}"; do RIDX["${R_KEY[$i]}"]=$i; done
    TOTAL_STEPS=${#R_KEY[@]}

    # Build display list (group headers interleaved) + step->line map
    local last="" line=0
    for i in "${!R_KEY[@]}"; do
        if [[ "${R_GROUP[$i]}" != "$last" ]]; then
            DISPLAY_KIND+=("group"); DISPLAY_TEXT+=("${R_GROUP[$i]}"); DISPLAY_STEPIDX+=("-1")
            last="${R_GROUP[$i]}"; ((line++))
        fi
        DISPLAY_KIND+=("step"); DISPLAY_TEXT+=("${R_NAME[$i]}"); DISPLAY_STEPIDX+=("$i")
        STEP_DISP_LINE[$i]=$line; ((line++))
    done
    DISPLAY_LEN=${#DISPLAY_KIND[@]}
    STATUS_SEP_ROW=$((LIST_START + DISPLAY_LEN))
    STATUS_ROW=$((STATUS_SEP_ROW + 1))
}

step_row() { echo $((LIST_START + STEP_DISP_LINE[$1])); }

glyph_for() {
    case "$1" in
        pending) printf "${DIM}${DOT_OFF}${RESET}" ;;
        running) printf "${BR_YELLOW}${RUN_GLYPH}${RESET}" ;;
        done)    printf "${BR_GREEN}${CHECK}${RESET}" ;;
        failed)  printf "${BR_RED}${CROSS}${RESET}" ;;
        skipped) printf "${DIM}${SKIP_GLYPH}${RESET}" ;;
    esac
}

draw_step_line() {
    local idx="$1" row; row=$(step_row "$idx")
    local status="${R_STATUS[$idx]}" name="${R_NAME[$idx]}"
    local maxw=$((TERM_COLS - 12)); (( maxw < 10 )) && maxw=10
    (( ${#name} > maxw )) && name="${name:0:maxw-1}…"
    move_to "$row" 1; clear_line
    local g; g=$(glyph_for "$status")
    case "$status" in
        pending) printf "      %b  ${DIM}%s${RESET}" "$g" "$name" ;;
        running) printf "      %b  ${BOLD}%s${RESET} ${DIM}${YELLOW}...${RESET}" "$g" "$name" ;;
        done)    printf "      %b  ${WHITE}%s${RESET}" "$g" "$name" ;;
        failed)  printf "      %b  ${BR_RED}%s${RESET}" "$g" "$name" ;;
        skipped) printf "      %b  ${DIM}%s${RESET}" "$g" "$name" ;;
    esac
}

set_status() {
    LAST_STATUS="$1"
    move_to "$STATUS_ROW" 1; clear_line
    printf "  ${DIM}${ARROW} ${WHITE}%s${RESET}" "$1"
}

render_progress() {
    if (( TERM_COLS < 54 || TERM_LINES < 18 )); then too_small; return; fi
    clear_screen
    draw_box "$SETUP_BOX_TITLE" "$BR_CYAN"
    draw_bar $(( TOTAL_STEPS > 0 ? CURRENT_STEP * 100 / TOTAL_STEPS : 0 ))
    local d row=$LIST_START
    for d in "${!DISPLAY_KIND[@]}"; do
        local kind="${DISPLAY_KIND[$d]}" text="${DISPLAY_TEXT[$d]}" sidx="${DISPLAY_STEPIDX[$d]}"
        if [[ "$kind" == "group" ]]; then
            move_to "$row" 1; clear_line
            printf "  ${BR_CYAN}${BOLD}${BAR} %s${RESET}" "$text"
        else
            draw_step_line "$sidx"
        fi
        ((row++))
    done
    move_to "$STATUS_SEP_ROW" 1; clear_line
    printf "  ${DIM}${CYAN}"; hr "─" $((TERM_COLS - 4)); printf "${RESET}"
    set_status "$LAST_STATUS"
    draw_timer
}

# ============================================================
# SPINNER
# ============================================================
SPINNER_PID=""
start_spinner() {
    local idx="$1" row
    local name="${R_NAME[$idx]}"
    row=$(step_row "$idx")
    (
        local maxw=$((TERM_COLS - 12)); (( maxw < 10 )) && maxw=10
        local n="$name"; (( ${#n} > maxw )) && n="${n:0:maxw-1}…"
        while true; do
            for ch in "${SPINNER_CHARS[@]}"; do
                move_to "$row" 1; clear_line
                printf "      ${BR_YELLOW}%s${RESET}  ${BOLD}%s${RESET} ${DIM}${YELLOW}...${RESET}" "$ch" "$n"
                draw_timer
                sleep 0.08
            done
        done
    ) &
    SPINNER_PID=$!
}
stop_spinner() {
    if [[ -n "$SPINNER_PID" ]] && kill -0 "$SPINNER_PID" 2>/dev/null; then
        kill "$SPINNER_PID" 2>/dev/null
        wait "$SPINNER_PID" 2>/dev/null || true
    fi
    SPINNER_PID=""
}

# ============================================================
# STEP RUNNER
# ============================================================
run_step() {
    local idx="$1"; shift
    R_STATUS[$idx]="running"
    RUNNING_IDX="$idx"
    draw_step_line "$idx"
    set_status "Installing: ${R_NAME[$idx]}"
    start_spinner "$idx"

    log "STEP ${R_KEY[$idx]}: ${R_NAME[$idx]}"
    # Run the worker in the background and wait on it. A foreground
    # external command would defer the SIGWINCH trap until it finished,
    # so the screen wouldn't redraw on resize mid-step; wait IS
    # interruptible by traps, so on_winch can repaint immediately.
    if (( DEBUG )); then
        log "DEBUG: not running $*"
        sleep 1 &
    else
        "$@" >> "$LOG_FILE" 2>&1 &
    fi
    local worker=$! rc
    while true; do
        wait "$worker"; rc=$?
        (( rc == 0 )) && break
        (( rc > 128 )) && kill -0 "$worker" 2>/dev/null && continue  # trap interrupted us
        break
    done

    RUNNING_IDX=""
    stop_spinner
    if (( rc == 0 )); then
        R_STATUS[$idx]="done"
        CURRENT_STEP=$((CURRENT_STEP + 1))
        draw_bar $(( CURRENT_STEP * 100 / TOTAL_STEPS ))
        draw_step_line "$idx"
        set_status "Completed: ${R_NAME[$idx]}"
        log "STEP ${R_KEY[$idx]}: SUCCESS"
    else
        R_STATUS[$idx]="failed"
        draw_step_line "$idx"
        set_status "FAILED: ${R_NAME[$idx]}   |   Log: $LOG_FILE"
        log "STEP ${R_KEY[$idx]}: FAILED (exit $rc)"
        show_cursor
        move_to $((STATUS_ROW + 2)) 1
        exit 1
    fi
}
skip_step() {
    local idx="$1"
    R_STATUS[$idx]="skipped"
    CURRENT_STEP=$((CURRENT_STEP + 1))
    draw_bar $(( CURRENT_STEP * 100 / TOTAL_STEPS ))
    draw_step_line "$idx"
    set_status "Skipped: ${R_NAME[$idx]}"
    log "STEP ${R_KEY[$idx]}: SKIPPED"
}

# ============================================================
# STEP FUNCTIONS
# ============================================================
ensure_local_bin_path() {
    local bashrc="${USER_HOME}/.bashrc"
    if ! grep -q '.local/bin' "$bashrc" 2>/dev/null; then
        echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$bashrc"
        chown "${SETUP_USERNAME}:" "$bashrc"
    fi
}

step_update() {
    apt update -y
    apt upgrade -y
}
step_essentials() {
    apt install -y build-essential net-tools btop git ca-certificates gnupg lsb-release
}
step_nodejs() {
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
    apt install -y nodejs
}
step_docker() {
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg
    chmod a+r /etc/apt/keyrings/docker.gpg
    echo \
      "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu \
      $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
      tee /etc/apt/sources.list.d/docker.list > /dev/null
    apt update
    apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}
step_create_user() {
    if ! id "$SETUP_USERNAME" &>/dev/null; then useradd -m -s /bin/bash "$SETUP_USERNAME"; fi
    echo "${SETUP_USERNAME}:${SETUP_PASSWORD}" | chpasswd
    usermod -aG sudo "$SETUP_USERNAME"
    usermod -aG docker "$SETUP_USERNAME"
}
step_ensure_docker_group() { usermod -aG docker "$SETUP_USERNAME"; }
step_configure_npm_global() {
    su - "$SETUP_USERNAME" -c 'mkdir -p ~/.npm-global && npm config set prefix "~/.npm-global"'
    local bashrc="${USER_HOME}/.bashrc"
    if ! grep -q '.npm-global/bin' "$bashrc" 2>/dev/null; then
        echo 'export PATH="$HOME/.npm-global/bin:$PATH"' >> "$bashrc"
        chown "${SETUP_USERNAME}:" "$bashrc"
    fi
}
step_claude() {
    su - "$SETUP_USERNAME" -c 'curl -fsSL https://claude.ai/install.sh | bash'
    ensure_local_bin_path
}
step_codex() {
    su - "$SETUP_USERNAME" -c 'npm install -g @openai/codex'
}
step_antigravity() {
    su - "$SETUP_USERNAME" -c 'curl -fsSL https://antigravity.google/cli/install.sh | bash'
    ensure_local_bin_path
}
step_ssh_keys() {
    local ssh_dir="${USER_HOME}/.ssh"
    if ! valid_ssh_keys "$SSH_KEYS"; then
        echo "No valid SSH public key to install (source: ${SSH_KEY_SOURCE})"
        return 1
    fi
    mkdir -p "$ssh_dir"
    printf '%s\n' "$SSH_KEYS" > "$ssh_dir/authorized_keys"
    chmod 700 "$ssh_dir"
    chmod 600 "$ssh_dir/authorized_keys"
    chown -R "${SETUP_USERNAME}:" "$ssh_dir"
}
step_ssh_hardening() {
    local config="/etc/ssh/sshd_config"
    # Never disable password login unless a usable key is in place
    if ! ssh-keygen -lf "${USER_HOME}/.ssh/authorized_keys" >/dev/null 2>&1; then
        echo "Refusing to harden SSH: no valid key in ${USER_HOME}/.ssh/authorized_keys"
        return 1
    fi
    sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/' "$config"
    sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' "$config"
    if [ -d /etc/ssh/sshd_config.d ]; then
        for f in /etc/ssh/sshd_config.d/*.conf; do
            [ -f "$f" ] || continue
            sed -i 's/^PasswordAuthentication yes/PasswordAuthentication no/' "$f"
        done
    fi
    systemctl restart ssh
}
step_timezone() { timedatectl set-timezone "$SETUP_TIMEZONE"; }
step_hostname() { [[ -n "$NEW_HOSTNAME" ]] && hostnamectl set-hostname "$NEW_HOSTNAME"; return 0; }
step_qemu_guest() {
    apt install -y qemu-guest-agent
    systemctl enable qemu-guest-agent
    systemctl start qemu-guest-agent
}
step_speedtest() { snap install speedtest; }
step_tailscale() { curl -fsSL https://tailscale.com/install.sh | sh; }

# ============================================================
# COMPLETION SCREEN
# ============================================================
render_complete() {
    clear_screen
    draw_box "SETUP COMPLETE" "$BR_GREEN"
    echo ""
    printf "  ${BR_GREEN}${CHECK}${RESET} ${BOLD}All tasks finished${RESET}\n\n"

    local i last=""
    for i in "${!R_KEY[@]}"; do
        if [[ "${R_GROUP[$i]}" != "$last" ]]; then
            printf "  ${BR_CYAN}${BOLD}${BAR} %s${RESET}\n" "${R_GROUP[$i]}"
            last="${R_GROUP[$i]}"
        fi
        if [[ "${R_STATUS[$i]}" == "skipped" ]]; then
            printf "      ${DIM}${SKIP_GLYPH}  %s  (skipped)${RESET}\n" "${R_NAME[$i]}"
        else
            printf "      ${BR_GREEN}${CHECK}${RESET}  %s\n" "${R_NAME[$i]}"
        fi
    done

    echo ""
    printf "  ${DIM}${CYAN}"; hr "─" 50; printf "${RESET}\n"
    [[ -n "$NEW_HOSTNAME" ]] && printf "  ${BOLD}Hostname${RESET}    %s\n" "$NEW_HOSTNAME"
    printf "  ${BOLD}User${RESET}        ${SETUP_USERNAME} (sudo + docker)\n"
    printf "  ${BOLD}SSH${RESET}         Key only, root disabled\n"
    printf "  ${BOLD}Timezone${RESET}    %s\n" "$SETUP_TIMEZONE"
    local now; printf -v now '%(%s)T' -1
    printf "  ${BOLD}Total time${RESET}  %s\n" "$(fmt_elapsed $(( now - START_EPOCH )))"
    printf "  ${BOLD}Log${RESET}         %s\n\n" "$LOG_FILE"
    printf "  ${BOLD}Connect${RESET}     ${BR_CYAN}ssh ${SETUP_USERNAME}@$(hostname -I 2>/dev/null | awk '{print $1}')${RESET}\n\n"
    printf "  ${DIM}${CYAN}"; hr "─" 50; printf "${RESET}\n"
    if (( DEBUG )); then
        printf "  ${BR_YELLOW}Debug run: nothing was installed or changed, not rebooting.${RESET}\n\n"
    else
        printf "  ${BR_YELLOW}Rebooting in 10 seconds...${RESET}\n\n"
    fi
}

# ============================================================
# MAIN FLOW
# ============================================================
clear_screen
hide_cursor

# Detect an existing non-root sudo user
DETECTED_USER="${SUDO_USER:-}"
[[ "$DETECTED_USER" == "root" ]] && DETECTED_USER=""
if [[ -n "$DETECTED_USER" ]] && ! id "$DETECTED_USER" &>/dev/null; then DETECTED_USER=""; fi

# Consolidated options form
build_form
run_form

# Derive account settings
if [[ "${FV[CREATE_USER]}" == "1" ]]; then
    SETUP_USERNAME="${FV[USERNAME]}"
    SETUP_PASSWORD="${FV[PASSWORD]}"
    SKIP_USER_CREATION=0
else
    if [[ -n "$DETECTED_USER" ]]; then SETUP_USERNAME="$DETECTED_USER"; else SETUP_USERNAME="${FV[USERNAME]}"; fi
    SETUP_PASSWORD=""
    SKIP_USER_CREATION=1
fi
GITHUB_USER="${FV[GITHUB]}"
NEW_HOSTNAME="${FV[HOSTNAME]}"
SETUP_TIMEZONE="${FV[TIMEZONE]}"

# SSH key source (GitHub keys were fetched and validated by the form)
SSH_KEY_SOURCE="github"
SSH_KEYS="$GITHUB_KEYS"
SSH_PASTE_ERROR=""
if [[ -z "$GITHUB_USER" ]]; then
    SSH_KEY_SOURCE="manual"
    SSH_KEYS=""
    CURRENT_SCREEN="sshpaste"
    render_sshpaste
    while [[ -z "$SSH_KEYS" ]]; do
        local pasted=""
        IFS= read -r pasted || { render_sshpaste; continue; }
        if valid_ssh_keys "$pasted"; then
            SSH_KEYS="$pasted"
        else
            [[ -n "$pasted" ]] && SSH_PASTE_ERROR="Not a valid SSH public key. Paste the full single-line .pub key."
            render_sshpaste
        fi
    done
    hide_cursor
fi
CURRENT_SCREEN=""

# Log selections
log "User: '${SETUP_USERNAME}'  Skip-create: ${SKIP_USER_CREATION}  SSH: ${SSH_KEY_SOURCE}"
log "Hostname: '${NEW_HOSTNAME:-<unchanged>}'  Timezone: '${SETUP_TIMEZONE}'"
log "Agents: claude=${FV[AGENT_CLAUDE]} codex=${FV[AGENT_CODEX]} agy=${FV[AGENT_AGY]}"
log "Infra: qemu=${FV[QEMU]} tailscale=${FV[TAILSCALE]}"

# Build the grouped step registry from selections
build_registry

# Run
printf -v START_EPOCH '%(%s)T' -1
CURRENT_SCREEN="progress"
render_progress

run_step "${RIDX[UPDATE]}"     step_update
run_step "${RIDX[ESSENTIALS]}" step_essentials
run_step "${RIDX[NODE]}"       step_nodejs
run_step "${RIDX[DOCKER]}"     step_docker

if (( SKIP_USER_CREATION )); then
    skip_step "${RIDX[USER]}"
    (( DEBUG )) || step_ensure_docker_group >> "$LOG_FILE" 2>&1
else
    run_step "${RIDX[USER]}" step_create_user
fi

# Resolve the real home directory (may not be /home/<user> for existing accounts)
USER_HOME="$(getent passwd "$SETUP_USERNAME" | cut -d: -f6)"
if [[ -z "$USER_HOME" || ! -d "$USER_HOME" ]] && (( ! DEBUG )); then
    set_status "FAILED: no home directory for ${SETUP_USERNAME}   |   Log: $LOG_FILE"
    log "No home directory found for ${SETUP_USERNAME} (got '${USER_HOME}')"
    show_cursor
    move_to $((STATUS_ROW + 2)) 1
    exit 1
fi

# Internal (not displayed): npm global prefix so the user never needs sudo
log "Configuring npm global prefix for ${SETUP_USERNAME}"
(( DEBUG )) || step_configure_npm_global >> "$LOG_FILE" 2>&1

# Move log into the user's home now that the account exists
NEW_LOG="${USER_HOME}/$(basename "$LOG_FILE")"
(( ! DEBUG )) && cp "$LOG_FILE" "$NEW_LOG" 2>/dev/null && { OLD_LOG="$LOG_FILE"; LOG_FILE="$NEW_LOG"; chown "${SETUP_USERNAME}:" "$LOG_FILE"; rm -f "$OLD_LOG"; }

run_step "${RIDX[SSHKEYS]}"   step_ssh_keys
run_step "${RIDX[SSHHARDEN]}" step_ssh_hardening

[[ -n "${RIDX[CLAUDE]:-}" ]] && run_step "${RIDX[CLAUDE]}" step_claude
[[ -n "${RIDX[CODEX]:-}"  ]] && run_step "${RIDX[CODEX]}"  step_codex
[[ -n "${RIDX[AGY]:-}"    ]] && run_step "${RIDX[AGY]}"    step_antigravity

run_step "${RIDX[TZ]}"        step_timezone
run_step "${RIDX[HOSTNAME]}"  step_hostname
run_step "${RIDX[SPEEDTEST]}" step_speedtest

[[ -n "${RIDX[QEMU]:-}"      ]] && run_step "${RIDX[QEMU]}"      step_qemu_guest
[[ -n "${RIDX[TAILSCALE]:-}" ]] && run_step "${RIDX[TAILSCALE]}" step_tailscale

# Finish
CURRENT_SCREEN=""
show_cursor
log "ALL STEPS COMPLETE. Rebooting."
render_complete
(( DEBUG )) && exit 0
sleep 10
reboot

} # end main()

main "$@"