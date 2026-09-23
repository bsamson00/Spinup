// spinup: interactive first-boot provisioning for fresh Ubuntu 24.04 (noble)
// and 26.04 (resolute) servers/VMs. Go port of setup.sh, stdlib only.
//
// Must be run as root:  sudo go run spinup.go   (or build: go build spinup.go)
// --debug: full UI walkthrough, but no step runs, nothing is changed, no reboot.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// ============================================================
// COLORS AND SYMBOLS
// ============================================================
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	reverse = "\033[7m"

	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	white  = "\033[37m"
	black  = "\033[90m"

	brRed    = "\033[91m"
	brGreen  = "\033[92m"
	brYellow = "\033[93m"
	brBlue   = "\033[94m"
	brCyan   = "\033[96m"
	brWhite  = "\033[97m"

	glyphCheck = "✔"
	glyphCross = "✖"
	arrow      = "▸"
	bar        = "▌"
	dotOn      = "◉"
	dotOff     = "○"
	glyphSkip  = "◇"
	glyphRun   = "◌"
	blockFull  = "█"
	blockLight = "░"
)

var spinnerChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	indent          = 4
	listStart       = 7
	minCols         = 54
	minLines        = 18
	defaultTimezone = "America/New_York"
)

// ============================================================
// GLOBAL STATE
// ============================================================
var (
	debug         bool
	isQemuGuest   bool
	setupBoxTitle string
	osCodename    string

	logPath string
	logFile *os.File

	setupUsername    string
	setupPassword    string
	skipUserCreation bool
	githubUser       string
	newHostname      string
	setupTimezone    string
	sshKeySource     string
	sshKeys          string
	sshPasteError    string
	userHome         string
)

// ============================================================
// LOGGING
// ============================================================
func logf(format string, a ...any) {
	fmt.Fprintf(logFile, "=== [%s] %s ===\n", time.Now().Format(time.UnixDate), fmt.Sprintf(format, a...))
}

// ============================================================
// TERMINAL (cbreak mode via stty, resize-aware)
// ============================================================
var (
	out       = bufio.NewWriterSize(os.Stdout, 1<<16)
	tty       *os.File
	ttySaved  string
	termCols  = 80
	termLines = 24
	winchCh   = make(chan os.Signal, 1)
	keyCh     = make(chan key, 256)
)

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = tty
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

// initTerminal reads keys from /dev/tty (so it works even when stdin is a
// pipe), turns off line buffering and echo, and starts the key reader and
// signal handlers. Ctrl-C still raises SIGINT for the whole foreground group.
func initTerminal() error {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	tty = f
	if ttySaved, err = stty("-g"); err != nil {
		return err
	}
	if _, err = stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return err
	}

	signal.Notify(winchCh, syscall.SIGWINCH)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() { <-sigCh; quit(130) }()

	updateDims()
	go readKeys(f)
	return nil
}

func restoreTerminal() {
	os.Stdout.WriteString("\033[?25h")
	if ttySaved != "" {
		stty(ttySaved)
		ttySaved = ""
	}
}

func quit(code int) {
	restoreTerminal()
	os.Exit(code)
}

func updateDims() {
	termCols, termLines = 80, 24
	if tty == nil {
		return
	}
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.Col > 0 && ws.Row > 0 {
		termCols, termLines = int(ws.Col), int(ws.Row)
	}
}

func isTooSmall() bool { return termCols < minCols || termLines < minLines }

// ============================================================
// KEY INPUT
// ============================================================
type keyKind int

const (
	keyNone keyKind = iota
	keyChar
	keyEnter
	keyTab
	keyBackTab
	keyBackspace
	keyEsc
	keyUp
	keyDown
	keyLeft
	keyRight
	keyPgUp
	keyPgDn
)

type key struct {
	kind keyKind
	ch   rune
}

func readKeys(f *os.File) {
	bytes := make(chan byte, 4096)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := f.Read(buf)
			for _, b := range buf[:n] {
				bytes <- b
			}
			if err != nil {
				close(bytes)
				return
			}
		}
	}()
	next := func() (byte, bool) {
		select {
		case b, ok := <-bytes:
			return b, ok
		case <-time.After(50 * time.Millisecond):
			return 0, false
		}
	}

	for b := range bytes {
		switch {
		case b == '\r' || b == '\n':
			keyCh <- key{kind: keyEnter}
		case b == '\t':
			keyCh <- key{kind: keyTab}
		case b == 0x7f || b == 0x08:
			keyCh <- key{kind: keyBackspace}
		case b == 0x1b:
			if k := parseEscape(next); k.kind != keyNone {
				keyCh <- k
			}
		case b >= 0x20 && b < 0x7f:
			keyCh <- key{kind: keyChar, ch: rune(b)}
		case b >= 0x80:
			buf := []byte{b}
			for !utf8.FullRune(buf) && len(buf) < utf8.UTFMax {
				c, ok := next()
				if !ok {
					break
				}
				buf = append(buf, c)
			}
			if r, _ := utf8.DecodeRune(buf); r != utf8.RuneError && unicode.IsPrint(r) {
				keyCh <- key{kind: keyChar, ch: r}
			}
		}
	}
}

// parseEscape decodes the bytes after ESC. A bare ESC (nothing follows
// within 50ms) is the Esc key; unknown sequences are consumed and dropped.
func parseEscape(next func() (byte, bool)) key {
	a, ok := next()
	if !ok {
		return key{kind: keyEsc}
	}
	if a != '[' && a != 'O' {
		return key{}
	}
	var seq []byte
	for {
		c, ok := next()
		if !ok || len(seq) > 16 {
			return key{}
		}
		seq = append(seq, c)
		if c >= 0x40 && c <= 0x7e {
			break
		}
	}
	switch string(seq) {
	case "A":
		return key{kind: keyUp}
	case "B":
		return key{kind: keyDown}
	case "C":
		return key{kind: keyRight}
	case "D":
		return key{kind: keyLeft}
	case "Z":
		return key{kind: keyBackTab}
	case "5~":
		return key{kind: keyPgUp}
	case "6~":
		return key{kind: keyPgDn}
	}
	return key{}
}

// nextEvent blocks for a key press or a terminal resize (resized == true).
func nextEvent() (k key, resized bool) {
	select {
	case <-winchCh:
		updateDims()
		return key{}, true
	case k := <-keyCh:
		return k, false
	}
}

// ============================================================
// OUTPUT HELPERS
// ============================================================
func w(parts ...string) {
	for _, p := range parts {
		out.WriteString(p)
	}
}

func hideCursor()         { out.WriteString("\033[?25l") }
func showCursor()         { out.WriteString("\033[?25h") }
func moveTo(row, col int) { fmt.Fprintf(out, "\033[%d;%dH", row, col) }
func clearLine()          { out.WriteString("\033[2K") }
func clearScreen()        { out.WriteString("\033[2J\033[H") }

// hr returns n copies of ch.
func hr(ch string, n int) string {
	if n < 1 {
		return ""
	}
	return strings.Repeat(ch, n)
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func padRight(s string, n int) string { return s + hr(" ", n-runeLen(s)) }

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func tooSmall() {
	clearScreen()
	moveTo(1, 1)
	w(brYellow, "Terminal too small.", reset, "\n")
	w(dim, fmt.Sprintf("Please enlarge the window (need at least %d cols x %d lines).", minCols, minLines), reset, "\n")
}

// ============================================================
// SHARED DRAWING
// ============================================================
func drawBox(title, color string) {
	inner := termCols - 2
	w(color, bold, "╔", hr("═", inner), "╗", reset, "\n")
	pad := max((inner-runeLen(title))/2, 0)
	rpad := max(inner-pad-runeLen(title), 0)
	w(color, bold, "║", reset, hr(" ", pad), bold, brWhite, title, reset, hr(" ", rpad), color, bold, "║", reset, "\n")
	w(color, bold, "╚", hr("═", inner), "╝", reset, "\n")
}

func drawBar(percent int) {
	barWidth := max(termCols-20, 10)
	filled := percent * barWidth / 100
	moveTo(5, 1)
	clearLine()
	w("  ", bold, brWhite, fmt.Sprintf("%3d%%", percent), reset, " ", dim, cyan, "│", reset)
	for i := 0; i < filled; i++ {
		switch {
		case i < barWidth/3:
			w(brBlue, blockFull, reset)
		case i < barWidth*2/3:
			w(brCyan, blockFull, reset)
		default:
			w(brGreen, blockFull, reset)
		}
	}
	w(dim, black, hr(blockLight, barWidth-filled), reset)
	w(dim, cyan, "│", reset)
}

// fmtElapsed formats a duration as M:SS or H:MM:SS.
func fmtElapsed(d time.Duration) string {
	e := max(int(d.Seconds()), 0)
	h, m, s := e/3600, (e%3600)/60, e%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// drawTimer draws the live elapsed-time clock at the top-right of the box.
func drawTimer() {
	if startTime.IsZero() {
		return
	}
	label := fmtElapsed(time.Since(startTime))
	moveTo(2, max(termCols-runeLen(label)-4, 1))
	w(brCyan, bold, "⏱ ", label, reset)
}

// ============================================================
// INPUT VALIDATION
// ============================================================
var (
	usernameRe   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	githubUserRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	hostLabelRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

func validUsername(u string) bool { return usernameRe.MatchString(u) && u != "root" }

func validGitHubUser(u string) bool { return githubUserRe.MatchString(u) }

func validHostname(h string) bool {
	if len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !hostLabelRe.MatchString(label) {
			return false
		}
	}
	return true
}

// sshFingerprints returns one `ssh-keygen -l` line per parseable key in text.
func sshFingerprints(text string) []string {
	if text == "" {
		return nil
	}
	cmd := exec.Command("ssh-keygen", "-lf", "/dev/stdin")
	cmd.Stdin = strings.NewReader(text + "\n")
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// validSSHKeys is true if the text contains at least one parseable SSH public key.
func validSSHKeys(text string) bool { return len(sshFingerprints(text)) > 0 }

func userExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}

// fetchGitHubKeys returns the body of github.com/<user>.keys, or "" on failure.
func fetchGitHubKeys(user string) string {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get("https://github.com/" + user + ".keys")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

// ============================================================
// PREFLIGHT FORM MODEL (single consolidated screen)
// ============================================================
type itemKind int

const (
	kindGroup itemKind = iota
	kindToggle
	kindNote
	kindText
	kindSecret
	kindSelect
	kindButton
)

type formItem struct {
	kind  itemKind
	key   string
	label string
}

var (
	formItems []formItem
	kindOf    = map[string]itemKind{}
	fv        = map[string]string{}
	rowOf     = map[string]int{}
	valColOf  = map[string]int{}
	valWOf    = map[string]int{}

	activeIdx    int
	formError    string
	formStatus   string
	detectedUser string
	githubKeys   string
)

func addItem(kind itemKind, key, label string) {
	formItems = append(formItems, formItem{kind, key, label})
	kindOf[key] = kind
}

func buildForm() {
	addItem(kindGroup, "G_ACCOUNT", "ACCOUNT")
	addItem(kindToggle, "CREATE_USER", "Create a new user account")
	addItem(kindNote, "USERNOTE", "Using existing user")
	addItem(kindText, "USERNAME", "Username")
	addItem(kindSecret, "PASSWORD", "Password")
	addItem(kindText, "GITHUB", "GitHub user (SSH keys)")
	addItem(kindText, "HOSTNAME", "Hostname (blank = keep)")
	addItem(kindSelect, "TIMEZONE", "Timezone")
	addItem(kindGroup, "G_AGENTS", "AI AGENTS")
	addItem(kindToggle, "AGENT_CLAUDE", "Claude Code")
	addItem(kindToggle, "AGENT_CODEX", "OpenAI Codex")
	addItem(kindToggle, "AGENT_AGY", "Google Antigravity (agy)")
	addItem(kindGroup, "G_INFRA", "INFRASTRUCTURE")
	addItem(kindToggle, "QEMU", "QEMU Guest Agent")
	addItem(kindToggle, "TAILSCALE", "Tailscale")
	addItem(kindButton, "RUN", "REVIEW & INSTALL")

	fv["TIMEZONE"] = defaultTimezone
	fv["AGENT_CLAUDE"] = "1"
	fv["AGENT_CODEX"] = "0"
	fv["AGENT_AGY"] = "0"
	fv["QEMU"] = "0"
	fv["TAILSCALE"] = "0"
	if detectedUser != "" {
		fv["CREATE_USER"] = "0"
	} else {
		fv["CREATE_USER"] = "1"
	}
}

func on(key string) bool { return fv[key] == "1" }

func fieldVisible(key string) bool {
	switch key {
	case "USERNAME":
		return on("CREATE_USER") || detectedUser == ""
	case "PASSWORD":
		return on("CREATE_USER")
	case "USERNOTE":
		return !on("CREATE_USER") && detectedUser != ""
	case "QEMU":
		return isQemuGuest
	}
	return true
}

func isFocusable(i int) bool {
	it := formItems[i]
	if it.kind == kindGroup || it.kind == kindNote {
		return false
	}
	return fieldVisible(it.key)
}

func focusNext() {
	n := len(formItems)
	j := activeIdx
	for c := 0; c < n; c++ {
		j = (j + 1) % n
		if isFocusable(j) {
			activeIdx = j
			return
		}
	}
}

func focusPrev() {
	n := len(formItems)
	j := activeIdx
	for c := 0; c < n; c++ {
		j = (j - 1 + n) % n
		if isFocusable(j) {
			activeIdx = j
			return
		}
	}
}

func isTextKind(k itemKind) bool { return k == kindText || k == kindSecret }

func drawFormValue(key string) {
	valCol, ok := valColOf[key]
	if !ok {
		return
	}
	valW, row := valWOf[key], rowOf[key]
	val := []rune(fv[key])
	var disp string
	if kindOf[key] == kindSecret {
		disp = hr(dotOn, min(len(val), valW))
	} else {
		if len(val) > valW {
			val = val[len(val)-valW:]
		}
		disp = string(val)
	}
	moveTo(row, valCol+2)
	w(padRight(disp, valW))
}

func drawFormField(i, row int) {
	it := formItems[i]
	active := i == activeIdx
	moveTo(row, 1)
	clearLine()

	marker := "  "
	if active {
		marker = brCyan + bold + arrow + " " + reset
	}

	switch it.kind {
	case kindButton:
		if active {
			w("  ", marker, reverse, brGreen, bold, "  ", arrow, " ", it.label, "  ", reset)
		} else {
			w("    ", dim, green, "[ ", reset, green, it.label, dim, green, " ]", reset)
		}
		return
	case kindToggle:
		box := dim + dotOff + reset
		if on(it.key) {
			box = brGreen + dotOn + reset
		}
		labelColor := white
		if active {
			labelColor = bold + brWhite
		}
		w("    ", marker, box, " ", labelColor, it.label, reset)
		return
	}

	// text / secret / select
	lbl := it.label
	if it.key == "USERNAME" && !on("CREATE_USER") {
		lbl = "Existing username"
	}
	labelW := 22
	if termCols < 72 {
		labelW = 15
	}
	if active {
		w("    ", marker, bold, brWhite, padRight(lbl, labelW), reset)
	} else {
		w("    ", marker, dim, padRight(lbl, labelW), reset)
	}

	valCol := indent + 4 + labelW + 1
	valW := max(min(termCols-valCol-4, 40), 8)
	valColOf[it.key], valWOf[it.key] = valCol, valW
	bc := dim + cyan
	if active {
		bc = brCyan
	}
	moveTo(row, valCol)
	w(bc, "[ ", reset)
	drawFormValue(it.key)
	moveTo(row, valCol+2+valW+1)
	w(bc, " ]", reset)
}

func placeFormCursor() {
	it := formItems[activeIdx]
	if !isTextKind(it.kind) {
		hideCursor()
		return
	}
	n := min(runeLen(fv[it.key]), valWOf[it.key])
	showCursor()
	moveTo(rowOf[it.key], valColOf[it.key]+2+n)
}

func renderForm() {
	if isTooSmall() {
		tooSmall()
		return
	}
	clearScreen()
	drawBox(setupBoxTitle, brCyan)
	row := 5
	moveTo(row, 1)
	w("  ", bold, brWhite, "Preflight Configuration", reset)
	row++
	moveTo(row, 1)
	w("  ", dim, cyan, hr("─", termCols-4), reset)
	row += 2

	for i, it := range formItems {
		switch it.kind {
		case kindGroup:
			row++
			moveTo(row, 1)
			w("  ", brCyan, bold, bar, " ", it.label, reset)
			row++
			continue
		case kindNote:
			if fieldVisible(it.key) {
				moveTo(row, 1)
				w("        ", dim, it.label, ": ", white, detectedUser, reset)
				row++
			}
			continue
		}
		if !fieldVisible(it.key) {
			continue
		}
		if it.kind == kindButton {
			row++
		}
		rowOf[it.key] = row
		drawFormField(i, row)
		row++
	}

	row++
	moveTo(row, 1)
	w("  ", dim, "Tab / ", arrow, arrow, " navigate    Space toggle / choose    Enter continue", reset)
	row++
	if formError != "" {
		moveTo(row, 1)
		clearLine()
		w("  ", brRed, formError, reset)
	} else if formStatus != "" {
		moveTo(row, 1)
		clearLine()
		w("  ", brYellow, formStatus, reset)
	}
	placeFormCursor()
}

func appendChar(key string, c rune) {
	if runeLen(fv[key]) < 64 {
		fv[key] += string(c)
	}
}

func deleteChar(key string) {
	if r := []rune(fv[key]); len(r) > 0 {
		fv[key] = string(r[:len(r)-1])
	}
}

func validateForm() bool {
	formError = ""
	u, gh, hn := fv["USERNAME"], fv["GITHUB"], fv["HOSTNAME"]
	fail := func(msg string) bool { formError = msg; return false }

	if on("CREATE_USER") {
		switch {
		case u == "":
			return fail("Username is required.")
		case !validUsername(u):
			return fail("Invalid username: a-z 0-9 _ - only, max 32, not root.")
		case fv["PASSWORD"] == "":
			return fail("Password is required.")
		}
	} else if detectedUser == "" {
		switch {
		case u == "":
			return fail("Enter an existing username, or enable 'Create a new user'.")
		case u == "root":
			return fail("Pick a non-root user.")
		case !userExists(u):
			return fail("User '" + u + "' does not exist.")
		}
	}
	if gh != "" && !validGitHubUser(gh) {
		return fail("Invalid GitHub username.")
	}
	if hn != "" && !validHostname(hn) {
		return fail("Invalid hostname: letters, digits, - and . only.")
	}
	if fv["TIMEZONE"] == "" {
		return fail("Timezone is required.")
	}

	// Fetch GitHub keys now so a user with no keys is caught before
	// anything runs (SSH hardening disables password login).
	githubKeys = ""
	if gh != "" {
		formStatus = "Fetching SSH keys from github.com/" + gh + ".keys ..."
		renderForm()
		out.Flush()
		formStatus = ""
		keys := fetchGitHubKeys(gh)
		if !validSSHKeys(keys) {
			return fail("No valid SSH keys at github.com/" + gh + ".keys (clear field to paste one).")
		}
		githubKeys = keys
	}
	return true
}

func runForm() {
	activeIdx = 0
	if !isFocusable(0) {
		focusNext()
	}
	renderForm()
	out.Flush()
	for {
		k, resized := nextEvent()
		if resized {
			renderForm()
			out.Flush()
			continue
		}
		it := formItems[activeIdx]
		switch k.kind {
		case keyEnter:
			switch {
			case isTextKind(it.kind):
				focusNext()
				renderForm()
			case it.kind == kindSelect:
				runTzPick()
			case validateForm():
				return
			default:
				renderForm()
			}
		case keyTab, keyDown:
			focusNext()
			renderForm()
		case keyUp, keyBackTab:
			focusPrev()
			renderForm()
		case keyBackspace:
			if isTextKind(it.kind) {
				deleteChar(it.key)
				drawFormValue(it.key)
				placeFormCursor()
			}
		case keyChar:
			switch {
			case k.ch == ' ' && it.kind == kindToggle:
				if on(it.key) {
					fv[it.key] = "0"
				} else {
					fv[it.key] = "1"
				}
				renderForm()
			case k.ch == ' ' && it.kind == kindSelect:
				runTzPick()
			case k.ch == ' ' && it.kind == kindButton:
				if validateForm() {
					return
				}
				renderForm()
			case isTextKind(it.kind):
				appendChar(it.key, k.ch)
				drawFormValue(it.key)
				placeFormCursor()
			}
		}
		out.Flush()
	}
}

// ============================================================
// TIMEZONE PICKER (type-to-filter list)
// ============================================================
var (
	tzList   []string
	tzMatch  []string
	tzFilter string
	tzSel    int
	tzTop    int
)

func loadTimezones() {
	if len(tzList) > 0 {
		return
	}
	if b, err := exec.Command("timedatectl", "list-timezones").Output(); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				tzList = append(tzList, l)
			}
		}
	}
	if len(tzList) == 0 {
		tzList = []string{defaultTimezone, "UTC"}
	}
}

func tzApplyFilter() {
	f := strings.ToLower(tzFilter)
	tzMatch = tzMatch[:0]
	for _, z := range tzList {
		if f == "" || strings.Contains(strings.ToLower(z), f) {
			tzMatch = append(tzMatch, z)
		}
	}
	tzSel, tzTop = 0, 0
}

func tzPageSize() int { return max(termLines-12, 1) }

func renderTzPick() {
	if isTooSmall() {
		tooSmall()
		return
	}
	clearScreen()
	drawBox(setupBoxTitle, brCyan)
	moveTo(5, 1)
	w("  ", bold, brWhite, "Select Timezone", reset)
	moveTo(6, 1)
	w("  ", dim, cyan, hr("─", termCols-4), reset)
	moveTo(8, 1)
	w("    ", dim, "Filter", reset, "  ", brCyan, "[ ", reset, tzFilter, brCyan, " ]", reset)

	n, row, visible := len(tzMatch), 10, tzPageSize()
	if tzSel < tzTop {
		tzTop = tzSel
	}
	if tzSel >= tzTop+visible {
		tzTop = tzSel - visible + 1
	}
	if n == 0 {
		moveTo(row, 1)
		w("      ", dim, "No matching timezones", reset)
	}
	for i := tzTop; i < n && i < tzTop+visible; i++ {
		moveTo(row, 1)
		if i == tzSel {
			w("    ", brCyan, bold, arrow, " ", brWhite, tzMatch[i], reset)
		} else {
			w("      ", white, tzMatch[i], reset)
		}
		row++
	}
	moveTo(termLines-1, 1)
	w("  ", dim, fmt.Sprintf("Type to filter    Up/Down move    Enter select    Esc cancel    (%d/%d)", n, len(tzList)), reset)
}

func runTzPick() {
	loadTimezones()
	hideCursor()
	tzFilter = ""
	tzApplyFilter()
	for i, z := range tzMatch {
		if z == fv["TIMEZONE"] {
			tzSel = i
			break
		}
	}
	renderTzPick()
	out.Flush()
loop:
	for {
		k, resized := nextEvent()
		if !resized {
			last := len(tzMatch) - 1
			switch k.kind {
			case keyEnter:
				if len(tzMatch) > 0 {
					fv["TIMEZONE"] = tzMatch[tzSel]
					break loop
				}
			case keyEsc:
				break loop
			case keyUp:
				tzSel = max(tzSel-1, 0)
			case keyDown:
				tzSel = max(min(tzSel+1, last), 0)
			case keyPgUp:
				tzSel = max(tzSel-tzPageSize(), 0)
			case keyPgDn:
				tzSel = max(min(tzSel+tzPageSize(), last), 0)
			case keyBackspace:
				if r := []rune(tzFilter); len(r) > 0 {
					tzFilter = string(r[:len(r)-1])
					tzApplyFilter()
				}
			case keyChar:
				if runeLen(tzFilter) < 40 {
					tzFilter += string(k.ch)
					tzApplyFilter()
				}
			}
		}
		renderTzPick()
		out.Flush()
	}
	renderForm()
}

// ============================================================
// SSH MANUAL PASTE SCREEN
// ============================================================
func renderSSHPaste() {
	clearScreen()
	drawBox(setupBoxTitle, brCyan)
	moveTo(6, 1)
	w("\n  ", bold, brWhite, "SSH Public Key", reset, "\n")
	w("  ", dim, cyan, hr("─", termCols-4), reset, "\n\n")
	w("  ", dim, "No GitHub username provided. Paste your SSH public key below.", reset, "\n")
	w("  ", dim, "(Typically starts with ssh-rsa, ssh-ed25519, or ecdsa-sha2)", reset, "\n\n")
	if sshPasteError != "" {
		w("  ", brRed, sshPasteError, reset, "\n\n")
	}
	w("  ", brCyan, arrow, reset, " ")
	showCursor()
}

// promptSSHKey reads lines until one contains a valid SSH public key.
func promptSSHKey() string {
	sshPasteError = ""
	var buf []rune
	renderSSHPaste()
	out.Flush()
	for {
		k, resized := nextEvent()
		if resized {
			renderSSHPaste()
			w(string(buf))
			out.Flush()
			continue
		}
		switch k.kind {
		case keyEnter:
			pasted := string(buf)
			if validSSHKeys(pasted) {
				hideCursor()
				return pasted
			}
			if pasted != "" {
				sshPasteError = "Not a valid SSH public key. Paste the full single-line .pub key."
			}
			buf = nil
			renderSSHPaste()
		case keyBackspace:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				w("\b \b")
			}
		case keyTab:
			buf = append(buf, '\t')
			w("\t")
		case keyChar:
			buf = append(buf, k.ch)
			w(string(k.ch))
		}
		out.Flush()
	}
}

// ============================================================
// REVIEW / CONFIRM SCREEN
// ============================================================
var summaryInstall bool // defaults to Back so a stray Enter can't start

func summaryLine(label, value string) {
	w("      ", dim, padRight(label, 14), reset, value, "\n")
}

func renderSummary() {
	if isTooSmall() {
		tooSmall()
		return
	}
	clearScreen()
	drawBox(setupBoxTitle, brCyan)
	moveTo(5, 1)
	w("  ", bold, brWhite, "Review & Confirm", reset, "\n")
	w("  ", dim, cyan, hr("─", termCols-4), reset, "\n\n")

	userLine := setupUsername + " (new, sudo + docker)"
	if skipUserCreation {
		userLine = setupUsername + " (existing, keys replaced)"
	}
	fps := sshFingerprints(sshKeys)
	var sshLine string
	if sshKeySource == "github" {
		sshLine = fmt.Sprintf("github.com/%s (%d key(s))", githubUser, len(fps))
	} else if len(fps) > 0 {
		// "256 SHA256:xxx comment (ED25519)" -> "pasted ED25519 SHA256:xxx"
		f := strings.Fields(fps[0])
		if len(f) >= 2 {
			sshLine = "pasted " + strings.Trim(f[len(f)-1], "()") + " " + f[1]
		}
	}
	hostLine := newHostname
	if hostLine == "" {
		h, _ := os.Hostname()
		hostLine = "unchanged (" + h + ")"
	}
	var agents, infra []string
	if on("AGENT_CLAUDE") {
		agents = append(agents, "Claude Code")
	}
	if on("AGENT_CODEX") {
		agents = append(agents, "OpenAI Codex")
	}
	if on("AGENT_AGY") {
		agents = append(agents, "Antigravity")
	}
	if on("QEMU") && isQemuGuest {
		infra = append(infra, "QEMU Guest Agent")
	}
	if on("TAILSCALE") {
		infra = append(infra, "Tailscale")
	}
	orNone := func(s []string) string {
		if len(s) == 0 {
			return "none"
		}
		return strings.Join(s, ", ")
	}

	w("  ", brCyan, bold, bar, " ACCOUNT & ACCESS", reset, "\n")
	summaryLine("User", userLine)
	summaryLine("SSH keys", sshLine)
	summaryLine("SSH", "root login and password auth disabled")
	w("\n  ", brCyan, bold, bar, " SYSTEM", reset, "\n")
	summaryLine("Hostname", hostLine)
	summaryLine("Timezone", setupTimezone)
	summaryLine("Installs", "updates, build tools, latest Node.js LTS, Docker, Speedtest")
	w("\n  ", brCyan, bold, bar, " OPTIONAL", reset, "\n")
	summaryLine("AI agents", orNone(agents))
	summaryLine("Infra", orNone(infra))
	w("\n")
	if debug {
		w("  ", brYellow, "Debug mode: nothing will be installed or changed.", reset, "\n\n")
	} else {
		w("  ", brYellow, "The machine reboots automatically when setup finishes.", reset, "\n\n")
	}

	w("   ")
	if summaryInstall {
		w(" ", reverse, brGreen, bold, "  ", arrow, " INSTALL  ", reset, " ")
		w("  ", dim, "[ ", reset, "BACK", dim, " ]", reset, "  ")
	} else {
		w("  ", dim, green, "[ ", reset, green, "INSTALL", dim, green, " ]", reset, "  ")
		w(" ", reverse, brCyan, bold, "  ", arrow, " BACK  ", reset, " ")
	}
	w("\n\n  ", dim, "Left/Right or Tab choose    Enter confirm    Esc back", reset)
}

// runSummary returns true to install, false to go back to the form.
func runSummary() bool {
	summaryInstall = false
	hideCursor()
	renderSummary()
	out.Flush()
	for {
		k, resized := nextEvent()
		if !resized {
			switch k.kind {
			case keyEnter:
				return summaryInstall
			case keyEsc:
				return false
			case keyTab, keyLeft, keyRight, keyBackTab:
				summaryInstall = !summaryInstall
			}
		}
		renderSummary()
		out.Flush()
	}
}

// ============================================================
// STEP REGISTRY (built from selections) + grouped progress
// ============================================================
type stepStatus int

const (
	statusPending stepStatus = iota
	statusRunning
	statusDone
	statusFailed
	statusSkipped
)

type step struct {
	group, name, key string
	status           stepStatus
}

type displayLine struct {
	isGroup bool
	text    string
	stepIdx int
}

var (
	steps        []*step
	ridx         = map[string]int{}
	display      []displayLine
	stepDispLine = map[int]int{}
	statusSepRow int
	statusRow    int
	currentStep  int
	lastStatus   = "Starting..."
	startTime    time.Time
	spinnerFrame int
)

func addReg(group, name, key string) {
	steps = append(steps, &step{group: group, name: name, key: key})
}

func buildRegistry() {
	userDesc := setupUsername
	if skipUserCreation {
		userDesc += ", existing"
	}
	sshDesc := "manual key"
	if sshKeySource == "github" {
		sshDesc = "github.com/" + githubUser
	}

	addReg("SYSTEM BASE", "System Update & Upgrade", "UPDATE")
	addReg("SYSTEM BASE", "Essential Packages", "ESSENTIALS")
	addReg("SYSTEM BASE", "Node.js (latest LTS)", "NODE")
	addReg("SYSTEM BASE", "Docker Engine", "DOCKER")

	addReg("ACCOUNT & ACCESS", "User Account ("+userDesc+")", "USER")
	addReg("ACCOUNT & ACCESS", "SSH Keys ("+sshDesc+")", "SSHKEYS")
	addReg("ACCOUNT & ACCESS", "SSH Hardening", "SSHHARDEN")

	if on("AGENT_CLAUDE") {
		addReg("AI AGENTS", "Claude Code", "CLAUDE")
	}
	if on("AGENT_CODEX") {
		addReg("AI AGENTS", "OpenAI Codex", "CODEX")
	}
	if on("AGENT_AGY") {
		addReg("AI AGENTS", "Google Antigravity (agy)", "AGY")
	}

	addReg("SYSTEM CONFIG", "Timezone: "+setupTimezone, "TZ")
	addReg("SYSTEM CONFIG", "Hostname Configuration", "HOSTNAME")
	addReg("SYSTEM CONFIG", "Speedtest CLI", "SPEEDTEST")

	if on("QEMU") && isQemuGuest {
		addReg("INFRASTRUCTURE", "QEMU Guest Agent", "QEMU")
	}
	if on("TAILSCALE") {
		addReg("INFRASTRUCTURE", "Tailscale", "TAILSCALE")
	}

	// Build display list (group headers interleaved) + step->line map
	last := ""
	for i, s := range steps {
		ridx[s.key] = i
		if s.group != last {
			display = append(display, displayLine{isGroup: true, text: s.group, stepIdx: -1})
			last = s.group
		}
		stepDispLine[i] = len(display)
		display = append(display, displayLine{text: s.name, stepIdx: i})
	}
	statusSepRow = listStart + len(display)
	statusRow = statusSepRow + 1
}

func stepRow(idx int) int { return listStart + stepDispLine[idx] }

func stepNameWidth() int { return max(termCols-12, 10) }

func drawStepLine(idx int) {
	s := steps[idx]
	name := truncate(s.name, stepNameWidth())
	moveTo(stepRow(idx), 1)
	clearLine()
	switch s.status {
	case statusPending:
		w("      ", dim, dotOff, reset, "  ", dim, name, reset)
	case statusRunning:
		w("      ", brYellow, glyphRun, reset, "  ", bold, name, reset, " ", dim, yellow, "...", reset)
	case statusDone:
		w("      ", brGreen, glyphCheck, reset, "  ", white, name, reset)
	case statusFailed:
		w("      ", brRed, glyphCross, reset, "  ", brRed, name, reset)
	case statusSkipped:
		w("      ", dim, glyphSkip, reset, "  ", dim, name, reset)
	}
}

func drawSpinner(idx int) {
	moveTo(stepRow(idx), 1)
	clearLine()
	ch := spinnerChars[spinnerFrame%len(spinnerChars)]
	spinnerFrame++
	w("      ", brYellow, ch, reset, "  ", bold, truncate(steps[idx].name, stepNameWidth()), reset, " ", dim, yellow, "...", reset)
}

func setStatus(msg string) {
	lastStatus = msg
	moveTo(statusRow, 1)
	clearLine()
	w("  ", dim, arrow, " ", white, msg, reset)
}

func progressPercent() int {
	if len(steps) == 0 {
		return 0
	}
	return currentStep * 100 / len(steps)
}

func renderProgress() {
	if isTooSmall() {
		tooSmall()
		return
	}
	clearScreen()
	drawBox(setupBoxTitle, brCyan)
	drawBar(progressPercent())
	for d, line := range display {
		if line.isGroup {
			moveTo(listStart+d, 1)
			clearLine()
			w("  ", brCyan, bold, bar, " ", line.text, reset)
		} else {
			drawStepLine(line.stepIdx)
		}
	}
	moveTo(statusSepRow, 1)
	clearLine()
	w("  ", dim, cyan, hr("─", termCols-4), reset)
	setStatus(lastStatus)
	drawTimer()
}

// failExit leaves the progress screen visible and exits non-zero.
func failExit() {
	showCursor()
	moveTo(statusRow+2, 1)
	out.Flush()
	quit(1)
}

// ============================================================
// STEP RUNNER
// ============================================================
func runStep(key string, fn func(io.Writer) error) {
	idx := ridx[key]
	s := steps[idx]
	s.status = statusRunning
	drawStepLine(idx)
	setStatus("Installing: " + s.name)
	logf("STEP %s: %s", s.key, s.name)
	out.Flush()

	// Run the step in the background so the spinner, timer and resize
	// redraws keep going while it works.
	done := make(chan error, 1)
	go func() {
		if debug {
			logf("DEBUG: not running step %s", s.key)
			time.Sleep(time.Second)
			done <- nil
			return
		}
		done <- fn(logFile)
	}()

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	var err error
wait:
	for {
		select {
		case err = <-done:
			break wait
		case <-ticker.C:
			if !isTooSmall() {
				drawSpinner(idx)
				drawTimer()
			}
		case <-winchCh:
			updateDims()
			renderProgress()
		case <-keyCh:
			// ignore keystrokes while steps run
		}
		out.Flush()
	}

	if err == nil {
		s.status = statusDone
		currentStep++
		drawBar(progressPercent())
		drawStepLine(idx)
		setStatus("Completed: " + s.name)
		logf("STEP %s: SUCCESS", s.key)
		out.Flush()
		return
	}
	s.status = statusFailed
	drawStepLine(idx)
	setStatus("FAILED: " + s.name + "   |   Log: " + logPath)
	logf("STEP %s: FAILED (%v)", s.key, err)
	failExit()
}

func skipStep(key string) {
	idx := ridx[key]
	s := steps[idx]
	s.status = statusSkipped
	currentStep++
	drawBar(progressPercent())
	drawStepLine(idx)
	setStatus("Skipped: " + s.name)
	logf("STEP %s: SKIPPED", s.key)
	out.Flush()
}

// ============================================================
// STEP HELPERS
// ============================================================

// run executes a command with its output going to the log.
func run(lw io.Writer, stdin io.Reader, name string, args ...string) error {
	fmt.Fprintf(lw, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = lw, lw
	return cmd.Run()
}

func cmd(lw io.Writer, name string, args ...string) error { return run(lw, nil, name, args...) }

// shell runs a bash snippet (e.g. curl | bash) with pipefail.
func shell(lw io.Writer, script string) error {
	return cmd(lw, "bash", "-c", "set -o pipefail; "+script)
}

// asUser runs a command as the setup user with a login shell.
func asUser(lw io.Writer, script string) error {
	return cmd(lw, "su", "-", setupUsername, "-c", script)
}

func cmds(lw io.Writer, cs ...[]string) error {
	for _, c := range cs {
		if err := cmd(lw, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func lookupIDs(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	return uid, gid, err
}

// chownUser gives path (recursively) to the setup user and their login group.
func chownUser(path string) error {
	uid, gid, err := lookupIDs(setupUsername)
	if err != nil {
		return err
	}
	return filepath.WalkDir(path, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// ensureBashrcLine appends line to the user's .bashrc unless marker is present.
func ensureBashrcLine(marker, line string) error {
	bashrc := filepath.Join(userHome, ".bashrc")
	existing, _ := os.ReadFile(bashrc)
	if strings.Contains(string(existing), marker) {
		return nil
	}
	f, err := os.OpenFile(bashrc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		line = "\n" + line
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return chownUser(bashrc)
}

func ensureLocalBinPath() error {
	return ensureBashrcLine(".local/bin", `export PATH="$HOME/.local/bin:$PATH"`)
}

// ============================================================
// STEP FUNCTIONS
// ============================================================
func stepUpdate(lw io.Writer) error {
	return cmds(lw, []string{"apt", "update", "-y"}, []string{"apt", "upgrade", "-y"})
}

func stepEssentials(lw io.Writer) error {
	return cmd(lw, "apt", "install", "-y", "build-essential", "net-tools", "btop", "git", "ca-certificates", "gnupg", "lsb-release")
}

func stepNodejs(lw io.Writer) error {
	if err := shell(lw, "curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -"); err != nil {
		return err
	}
	return cmd(lw, "apt", "install", "-y", "nodejs")
}

func stepDocker(lw io.Writer) error {
	const keyring = "/etc/apt/keyrings/docker.gpg"
	if err := cmd(lw, "install", "-m", "0755", "-d", "/etc/apt/keyrings"); err != nil {
		return err
	}
	if err := shell(lw, "curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --batch --yes --dearmor -o "+keyring); err != nil {
		return err
	}
	if err := cmd(lw, "chmod", "a+r", keyring); err != nil {
		return err
	}
	arch, err := exec.Command("dpkg", "--print-architecture").Output()
	if err != nil {
		return err
	}
	repo := fmt.Sprintf("deb [arch=%s signed-by=%s] https://download.docker.com/linux/ubuntu %s stable\n",
		strings.TrimSpace(string(arch)), keyring, osCodename)
	if err := os.WriteFile("/etc/apt/sources.list.d/docker.list", []byte(repo), 0644); err != nil {
		return err
	}
	return cmds(lw,
		[]string{"apt", "update"},
		[]string{"apt", "install", "-y", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"},
	)
}

func stepCreateUser(lw io.Writer) error {
	if !userExists(setupUsername) {
		if err := cmd(lw, "useradd", "-m", "-s", "/bin/bash", setupUsername); err != nil {
			return err
		}
	}
	fmt.Fprintln(lw, "+ chpasswd")
	c := exec.Command("chpasswd")
	c.Stdin = strings.NewReader(setupUsername + ":" + setupPassword + "\n")
	c.Stdout, c.Stderr = lw, lw
	if err := c.Run(); err != nil {
		return err
	}
	return cmds(lw,
		[]string{"usermod", "-aG", "sudo", setupUsername},
		[]string{"usermod", "-aG", "docker", setupUsername},
	)
}

func stepEnsureDockerGroup(lw io.Writer) error {
	return cmd(lw, "usermod", "-aG", "docker", setupUsername)
}

func stepConfigureNpmGlobal(lw io.Writer) error {
	if err := asUser(lw, `mkdir -p ~/.npm-global && npm config set prefix "~/.npm-global"`); err != nil {
		return err
	}
	return ensureBashrcLine(".npm-global/bin", `export PATH="$HOME/.npm-global/bin:$PATH"`)
}

func stepClaude(lw io.Writer) error {
	if err := asUser(lw, "curl -fsSL https://claude.ai/install.sh | bash"); err != nil {
		return err
	}
	return ensureLocalBinPath()
}

func stepCodex(lw io.Writer) error {
	return asUser(lw, "npm install -g @openai/codex")
}

func stepAntigravity(lw io.Writer) error {
	if err := asUser(lw, "curl -fsSL https://antigravity.google/cli/install.sh | bash"); err != nil {
		return err
	}
	return ensureLocalBinPath()
}

func stepSSHKeys(lw io.Writer) error {
	if !validSSHKeys(sshKeys) {
		return fmt.Errorf("no valid SSH public key to install (source: %s)", sshKeySource)
	}
	sshDir := filepath.Join(userHome, ".ssh")
	authKeys := filepath.Join(sshDir, "authorized_keys")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(authKeys, []byte(sshKeys+"\n"), 0600); err != nil {
		return err
	}
	if err := os.Chmod(sshDir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(authKeys, 0600); err != nil {
		return err
	}
	return chownUser(sshDir)
}

var (
	rootLoginRe    = regexp.MustCompile(`(?m)^#*PermitRootLogin.*$`)
	passwordAuthRe = regexp.MustCompile(`(?m)^#*PasswordAuthentication.*$`)
	passwordYesRe  = regexp.MustCompile(`(?m)^PasswordAuthentication yes`)
)

func rewriteFile(path string, edit func(string) string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(edit(string(b))), 0644)
}

func stepSSHHardening(lw io.Writer) error {
	// Never disable password login unless a usable key is in place
	authKeys := filepath.Join(userHome, ".ssh", "authorized_keys")
	b, _ := os.ReadFile(authKeys)
	if !validSSHKeys(string(b)) {
		return fmt.Errorf("refusing to harden SSH: no valid key in %s", authKeys)
	}
	err := rewriteFile("/etc/ssh/sshd_config", func(s string) string {
		s = rootLoginRe.ReplaceAllString(s, "PermitRootLogin no")
		return passwordAuthRe.ReplaceAllString(s, "PasswordAuthentication no")
	})
	if err != nil {
		return err
	}
	confs, _ := filepath.Glob("/etc/ssh/sshd_config.d/*.conf")
	for _, f := range confs {
		if fi, err := os.Stat(f); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := rewriteFile(f, func(s string) string {
			return passwordYesRe.ReplaceAllString(s, "PasswordAuthentication no")
		}); err != nil {
			return err
		}
	}
	return cmd(lw, "systemctl", "restart", "ssh")
}

func stepTimezone(lw io.Writer) error {
	return cmd(lw, "timedatectl", "set-timezone", setupTimezone)
}

func stepHostname(lw io.Writer) error {
	if newHostname == "" {
		return nil
	}
	return cmd(lw, "hostnamectl", "set-hostname", newHostname)
}

func stepQemuGuest(lw io.Writer) error {
	return cmds(lw,
		[]string{"apt", "install", "-y", "qemu-guest-agent"},
		[]string{"systemctl", "enable", "qemu-guest-agent"},
		[]string{"systemctl", "start", "qemu-guest-agent"},
	)
}

func stepSpeedtest(lw io.Writer) error { return cmd(lw, "snap", "install", "speedtest") }

func stepTailscale(lw io.Writer) error {
	return shell(lw, "curl -fsSL https://tailscale.com/install.sh | sh")
}

// ============================================================
// COMPLETION SCREEN
// ============================================================
func primaryIP() string {
	b, err := exec.Command("hostname", "-I").Output()
	if err != nil {
		return ""
	}
	if f := strings.Fields(string(b)); len(f) > 0 {
		return f[0]
	}
	return ""
}

func renderComplete() {
	clearScreen()
	drawBox("SETUP COMPLETE", brGreen)
	w("\n  ", brGreen, glyphCheck, reset, " ", bold, "All tasks finished", reset, "\n\n")

	last := ""
	for _, s := range steps {
		if s.group != last {
			w("  ", brCyan, bold, bar, " ", s.group, reset, "\n")
			last = s.group
		}
		if s.status == statusSkipped {
			w("      ", dim, glyphSkip, "  ", s.name, "  (skipped)", reset, "\n")
		} else {
			w("      ", brGreen, glyphCheck, reset, "  ", s.name, "\n")
		}
	}

	w("\n  ", dim, cyan, hr("─", 50), reset, "\n")
	if newHostname != "" {
		w("  ", bold, "Hostname", reset, "    ", newHostname, "\n")
	}
	w("  ", bold, "User", reset, "        ", setupUsername, " (sudo + docker)\n")
	w("  ", bold, "SSH", reset, "         Key only, root disabled\n")
	w("  ", bold, "Timezone", reset, "    ", setupTimezone, "\n")
	w("  ", bold, "Total time", reset, "  ", fmtElapsed(time.Since(startTime)), "\n")
	w("  ", bold, "Log", reset, "         ", logPath, "\n\n")
	w("  ", bold, "Connect", reset, "     ", brCyan, "ssh ", setupUsername, "@", primaryIP(), reset, "\n\n")
	w("  ", dim, cyan, hr("─", 50), reset, "\n")
	if debug {
		w("  ", brYellow, "Debug run: nothing was installed or changed, not rebooting.", reset, "\n\n")
	} else {
		w("  ", brYellow, "Rebooting in 10 seconds...", reset, "\n\n")
	}
}

// ============================================================
// STARTUP
// ============================================================
func readOSRelease() (map[string]string, error) {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok && !strings.HasPrefix(k, "#") {
			m[k] = strings.Trim(v, `"'`)
		}
	}
	return m, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func fatal(format string, a ...any) {
	fmt.Printf(brRed+bold+"ERROR:"+reset+" "+format+"\n", a...)
	os.Exit(1)
}

// openLog creates the log next to the binary, falling back to /tmp.
func openLog() {
	name := "ubuntu-setup-" + time.Now().Format("20060102-150405") + ".log"
	dirs := []string{"/tmp"}
	if exe, err := os.Executable(); err == nil {
		dirs = append([]string{filepath.Dir(exe)}, dirs...)
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			logPath, logFile = p, f
			return
		}
	}
	fatal("could not create a log file")
}

// moveLog moves the log into the user's home now that the account exists.
func moveLog() {
	newPath := filepath.Join(userHome, filepath.Base(logPath))
	data, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	if err := os.WriteFile(newPath, data, 0644); err != nil {
		return
	}
	f, err := os.OpenFile(newPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	chownUser(newPath)
	logFile.Close()
	os.Remove(logPath)
	logPath, logFile = newPath, f
}

// ============================================================
// MAIN FLOW
// ============================================================
func main() {
	flag.BoolVar(&debug, "debug", false, "walk through the UI without making any changes")
	flag.Parse()

	// OS detection & compatibility guard: Ubuntu 24.04 (noble) and 26.04 (resolute) only.
	// In debug mode a missing /etc/os-release (e.g. macOS) is tolerated.
	osr, err := readOSRelease()
	if err != nil && !debug {
		fatal("Unsupported system.")
	}
	versionID := firstNonEmpty(osr["VERSION_ID"], "unknown")
	codename := firstNonEmpty(osr["UBUNTU_CODENAME"], osr["VERSION_CODENAME"], "unknown")
	osCodename = firstNonEmpty(osr["VERSION_CODENAME"], codename)
	setupBoxTitle = "UBUNTU " + versionID + " SERVER SETUP"
	if debug {
		setupBoxTitle += " [DEBUG]"
	}
	if codename != "noble" && codename != "resolute" && !debug {
		fmt.Printf(brRed + bold + "ERROR:" + reset + " This script supports Ubuntu 24.04 (noble) and 26.04 (resolute) only.\n")
		fmt.Printf("%sDetected: %s (codename: %s)%s\n", dim, firstNonEmpty(osr["PRETTY_NAME"], versionID), codename, reset)
		os.Exit(1)
	}

	// QEMU Guest Agent is only offered on a QEMU/KVM guest (e.g. a Proxmox VM).
	if b, err := exec.Command("systemd-detect-virt").Output(); err == nil {
		v := strings.TrimSpace(string(b))
		isQemuGuest = v == "qemu" || v == "kvm"
	}

	if os.Geteuid() != 0 && !debug {
		fatal("This script must be run as root (sudo).")
	}

	// Non-interactive apt / needrestart for every child process. needrestart
	// would otherwise pop a dialog that blocks apt; we reboot at the end.
	os.Setenv("DEBIAN_FRONTEND", "noninteractive")
	os.Setenv("NEEDRESTART_MODE", "a")
	os.Setenv("NEEDRESTART_SUSPEND", "1")

	openLog()
	fmt.Fprintf(logFile, "=== Ubuntu %s Setup Started: %s ===\n", versionID, time.Now().Format(time.UnixDate))

	if err := initTerminal(); err != nil {
		fatal("an interactive terminal is required (%v)", err)
	}
	clearScreen()
	hideCursor()

	// Detect an existing non-root sudo user
	detectedUser = os.Getenv("SUDO_USER")
	if detectedUser == "root" || (detectedUser != "" && !userExists(detectedUser)) {
		detectedUser = ""
	}

	buildForm()

	// Form -> (SSH key paste) -> review; "Back" on the review returns to the form
	for {
		runForm()

		if on("CREATE_USER") {
			setupUsername, setupPassword, skipUserCreation = fv["USERNAME"], fv["PASSWORD"], false
		} else {
			setupUsername = firstNonEmpty(detectedUser, fv["USERNAME"])
			setupPassword, skipUserCreation = "", true
		}
		githubUser = fv["GITHUB"]
		newHostname = fv["HOSTNAME"]
		setupTimezone = fv["TIMEZONE"]

		// SSH key source (GitHub keys were fetched and validated by the form)
		sshKeySource, sshKeys = "github", githubKeys
		if githubUser == "" {
			sshKeySource = "manual"
			sshKeys = promptSSHKey()
		}

		if runSummary() {
			break
		}
	}

	logf("User: '%s'  Skip-create: %t  SSH: %s", setupUsername, skipUserCreation, sshKeySource)
	logf("Hostname: '%s'  Timezone: '%s'", firstNonEmpty(newHostname, "<unchanged>"), setupTimezone)
	logf("Agents: claude=%s codex=%s agy=%s", fv["AGENT_CLAUDE"], fv["AGENT_CODEX"], fv["AGENT_AGY"])
	logf("Infra: qemu=%s tailscale=%s", fv["QEMU"], fv["TAILSCALE"])

	buildRegistry()

	startTime = time.Now()
	renderProgress()
	out.Flush()

	runStep("UPDATE", stepUpdate)
	runStep("ESSENTIALS", stepEssentials)
	runStep("NODE", stepNodejs)
	runStep("DOCKER", stepDocker)

	if skipUserCreation {
		skipStep("USER")
		if !debug {
			if err := stepEnsureDockerGroup(logFile); err != nil {
				logf("Adding %s to docker group failed: %v", setupUsername, err)
			}
		}
	} else {
		runStep("USER", stepCreateUser)
	}

	// Resolve the real home directory (may not be /home/<user> for existing accounts)
	userHome = ""
	if u, err := user.Lookup(setupUsername); err == nil {
		userHome = u.HomeDir
	}
	if fi, err := os.Stat(userHome); (userHome == "" || err != nil || !fi.IsDir()) && !debug {
		setStatus("FAILED: no home directory for " + setupUsername + "   |   Log: " + logPath)
		logf("No home directory found for %s (got '%s')", setupUsername, userHome)
		failExit()
	}

	if !debug {
		// Internal (not displayed): npm global prefix so the user never needs sudo
		logf("Configuring npm global prefix for %s", setupUsername)
		if err := stepConfigureNpmGlobal(logFile); err != nil {
			logf("npm global prefix setup failed: %v", err)
		}
		moveLog()
	}

	runStep("SSHKEYS", stepSSHKeys)
	runStep("SSHHARDEN", stepSSHHardening)

	optional := []struct {
		key string
		fn  func(io.Writer) error
	}{
		{"CLAUDE", stepClaude}, {"CODEX", stepCodex}, {"AGY", stepAntigravity},
		{"TZ", stepTimezone}, {"HOSTNAME", stepHostname}, {"SPEEDTEST", stepSpeedtest},
		{"QEMU", stepQemuGuest}, {"TAILSCALE", stepTailscale},
	}
	for _, o := range optional {
		if _, ok := ridx[o.key]; ok {
			runStep(o.key, o.fn)
		}
	}

	// Finish
	showCursor()
	logf("ALL STEPS COMPLETE. Rebooting.")
	renderComplete()
	out.Flush()
	restoreTerminal()
	if debug {
		return
	}
	time.Sleep(10 * time.Second)
	exec.Command("reboot").Run()
}
