// pick.go implements `submux pick` (spec §2): an interactive setup picker
// that replaces typing submux-claude's four --fable/--opus/--sonnet/--haiku
// flags with two screens -- a most-used-first history, and a no-id-typing
// create wizard. It renders to /dev/tty (never stdout) and hands its result
// back through a --out file the shell launcher sources (§2.1), which is
// what keeps this whole feature off submux's `serve` hot path: nothing here
// is reachable from proxying.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var tierKeys = [5]string{"main", "fable", "opus", "sonnet", "haiku"}
var tierLabels = [5]string{"main-loop model", "fable", "opus", "sonnet", "haiku"}

// cmdPick implements the `submux pick` command (spec §2.1).
func cmdPick(args []string) {
	fs := flag.NewFlagSet("pick", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json")
	outPath := fs.String("out", "", "file to write the chosen setup's KEY=VALUE lines to")
	_ = fs.Parse(args)

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}

	pPath, err := profilesPath()
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	cPath, err := modelsCachePath()
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	profiles, warning := loadProfiles(pPath)

	tty, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if ttyErr != nil {
		// No controlling terminal (spec §3): degrade to a plain numbered
		// list read from stdin, never hang, never touch the alt-screen.
		os.Exit(runPlainPicker(cfg, pPath, profiles, *outPath))
	}
	defer tty.Close()

	m := newPickModel(cfg, pPath, cPath, *outPath, profiles, warning)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(tty), tea.WithOutput(tty))
	final, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "submux pick: %v\n", err)
		os.Exit(1)
	}
	pm := final.(pickModel)
	if pm.launchProfile == nil {
		os.Exit(1) // q / Ctrl-C: exit non-zero, nothing written (§2.1)
	}
	os.Exit(0)
}

// ---- plain (non-TTY) fallback, spec §3 ----------------------------------

// runPlainPicker degrades to a numbered list on stdout/stdin when /dev/tty
// is unavailable (piped stdin, CI, `< /dev/null`). It never touches the
// alt-screen and never hangs: EOF on stdin (no selection available) exits
// 1 having written nothing.
func runPlainPicker(cfg *config, pPath string, profiles []Profile, outPath string) int {
	fmt.Fprintln(os.Stderr, "submux pick: no controlling terminal, falling back to a plain list")
	for i, p := range profiles {
		fmt.Fprintf(os.Stderr, "%d) %s (used %d time(s))\n", i+1, p.Name, p.Uses)
	}
	fmt.Fprintf(os.Stderr, "%d) create a new setup (not supported without a terminal)\n", len(profiles)+1)
	fmt.Fprint(os.Stderr, "select a number: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "submux pick: no input, nothing selected")
		return 1
	}
	line := strings.TrimSpace(scanner.Text())
	idx := -1
	fmt.Sscanf(line, "%d", &idx)
	if idx < 1 || idx > len(profiles) {
		fmt.Fprintln(os.Stderr, "submux pick: invalid selection")
		return 1
	}
	chosen := profiles[idx-1]
	profiles = touchProfile(profiles, chosen.Name, time.Now())
	if err := saveProfiles(pPath, profiles); err != nil {
		fmt.Fprintf(os.Stderr, "submux pick: save profiles: %v\n", err)
	}
	if outPath != "" {
		if err := writeOutFile(outPath, picksFromProfile(chosen)); err != nil {
			fmt.Fprintf(os.Stderr, "submux pick: write --out: %v\n", err)
			return 1
		}
	}
	return 0
}

// ---- --out file, spec §2.6 -----------------------------------------------

// picks is the five model-id slots a launch resolves to, plus the saved
// profile's name (empty for an ad-hoc, unsaved launch).
type picks struct {
	Main, Fable, Opus, Sonnet, Haiku string
	ProfileName                      string
}

func picksFromProfile(p Profile) picks {
	return picks{Main: p.Main, Fable: p.Fable, Opus: p.Opus, Sonnet: p.Sonnet, Haiku: p.Haiku, ProfileName: p.Name}
}

// writeOutFile writes exactly the §2.6 key names, only for slots that are
// set, as shell KEY=VALUE lines the launcher sources.
func writeOutFile(path string, p picks) error {
	var b strings.Builder
	writeIfSet := func(key, val string) {
		if val == "" {
			return
		}
		fmt.Fprintf(&b, "%s=%s\n", key, shellQuote(val))
	}
	writeIfSet("SUBMUX_MAIN", p.Main)
	writeIfSet("SUBMUX_FABLE", p.Fable)
	writeIfSet("SUBMUX_OPUS", p.Opus)
	writeIfSet("SUBMUX_SONNET", p.Sonnet)
	writeIfSet("SUBMUX_HAIKU", p.Haiku)
	writeIfSet("SUBMUX_PROFILE", p.ProfileName)
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// shellQuote wraps v in single quotes for safe `.` (source)-ing by a POSIX
// shell, escaping any embedded single quote.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// ---- bubbletea model ------------------------------------------------------

type pickMode int

const (
	modeHistory pickMode = iota
	modeWizSub
	modeWizModel
	modeWizName
)

type pickModel struct {
	cfg          *config
	profilesPath string
	cachePath    string
	outPath      string

	profiles []Profile
	warning  string

	mode         pickMode
	cursor       int
	filtering    bool
	filterInput  textinput.Model
	deleteTarget int // index into filtered history rows pending a second 'd', -1 = none

	catalog      []subscriptionOption
	cacheEntries map[string]cacheEntry
	catalogWarn  string

	spin          spinner.Model
	refreshing    bool   // a manual 'r' refresh (DEFECT 1, round 2) is in flight
	refreshTarget string // subscription Name being refreshed
	refreshErr    string // last refresh failure, surfaced in the status bar until the next refresh

	wizStep     int
	wizPicks    [5]string
	subCursor   int
	modelCursor int
	modelFilter textinput.Model
	nameInput   textinput.Model

	width, height int
	now           time.Time

	launchProfile *Profile
	quitting      bool
}

func newPickModel(cfg *config, pPath, cPath, outPath string, profiles []Profile, warning string) pickModel {
	fi := textinput.New()
	fi.Placeholder = "filter"
	mf := textinput.New()
	mf.Placeholder = "filter models"
	ni := textinput.New()
	ni.Placeholder = "setup name"
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	m := pickModel{
		cfg:          cfg,
		profilesPath: pPath,
		cachePath:    cPath,
		outPath:      outPath,
		profiles:     profiles,
		warning:      warning,
		filterInput:  fi,
		modelFilter:  mf,
		nameInput:    ni,
		deleteTarget: -1,
		now:          time.Now(),
		spin:         sp,
	}
	if len(profiles) == 0 {
		// Empty history jumps straight into the wizard (§2.2).
		m.mode = modeWizSub
		if m.warning == "" {
			m.warning = "no saved setups yet -- create your first one"
		}
	}
	return m
}

func (m pickModel) Init() tea.Cmd {
	return nil
}

// visibleProfiles returns profiles matching the active filter, in the
// already-sorted order they were loaded in.
func (m pickModel) visibleProfiles() []Profile {
	if !m.filtering && m.filterInput.Value() == "" {
		return m.profiles
	}
	q := strings.ToLower(m.filterInput.Value())
	if q == "" {
		return m.profiles
	}
	var out []Profile
	for _, p := range m.profiles {
		if strings.Contains(strings.ToLower(p.Name), q) {
			out = append(out, p)
		}
	}
	return out
}

func (m *pickModel) ensureCatalog() {
	if m.catalog != nil {
		return
	}
	entries := loadModelsCache(m.cachePath)
	m.catalog = buildSubscriptionCatalog(m.cfg, entries, m.now)
	m.cacheEntries = entries
	if err := saveModelsCache(m.cachePath, entries); err != nil {
		m.catalogWarn = fmt.Sprintf("could not write models cache: %v", err)
	}
}

func (m pickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case spinner.TickMsg:
		if !m.refreshing {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case refreshResultMsg:
		return m.applyRefreshResult(msg), nil
	}
	return m, nil
}

// applyRefreshResult merges a completed 'r'-triggered refresh (DEFECT 1,
// round 2) back into the model: successes update the disk cache and rebuild
// the catalog so the row's age clears; failures are surfaced in the status
// bar without touching m.cacheEntries, so the stale data already on screen
// is never blanked.
func (m pickModel) applyRefreshResult(msg refreshResultMsg) pickModel {
	m.refreshing = false
	if m.cacheEntries == nil {
		m.cacheEntries = map[string]cacheEntry{}
	}
	for up, entry := range msg.upstreams {
		m.cacheEntries[up] = entry
	}
	if len(msg.upstreams) > 0 {
		if err := saveModelsCache(m.cachePath, m.cacheEntries); err != nil {
			m.catalogWarn = fmt.Sprintf("could not write models cache: %v", err)
		}
		m.catalog = buildSubscriptionCatalog(m.cfg, m.cacheEntries, time.Now())
	}
	if len(msg.failedUpstreams) > 0 {
		errText := "refresh failed"
		if msg.firstErr != nil {
			errText = msg.firstErr.Error()
		}
		m.refreshErr = fmt.Sprintf("refresh of %s failed (%s) -- showing cached data", msg.subName, errText)
	} else {
		m.refreshErr = ""
	}
	for i, opt := range m.catalog {
		if opt.Name == msg.subName {
			m.subCursor = i
			break
		}
	}
	return m
}

func (m pickModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global quit: q only outside a text-entry field (so 'q' can be typed
	// into a filter/name box); Ctrl-C always quits.
	if msg.Type == tea.KeyCtrlC {
		m.quitting = true
		return m, tea.Quit
	}

	switch m.mode {
	case modeHistory:
		return m.updateHistory(msg)
	case modeWizSub:
		m.ensureCatalog()
		return m.updateWizSub(msg)
	case modeWizModel:
		return m.updateWizModel(msg)
	case modeWizName:
		return m.updateWizName(msg)
	}
	return m, nil
}

func (m pickModel) updateHistory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch msg.String() {
		case "esc":
			m.filtering = false
			m.filterInput.Blur()
			return m, nil
		case "enter":
			m.filtering = false
			m.filterInput.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.cursor = 0
		return m, cmd
	}

	rows := m.visibleProfiles()
	newSetupIdx := len(rows)

	if m.deleteTarget >= 0 {
		if msg.String() == "d" && m.deleteTarget == m.cursor {
			name := rows[m.deleteTarget].Name
			m.profiles = removeProfileByName(m.profiles, name)
			_ = saveProfiles(m.profilesPath, m.profiles)
			m.deleteTarget = -1
			if m.cursor >= len(m.visibleProfiles()) && m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		}
		m.deleteTarget = -1 // any other key cancels the confirm
		return m, nil
	}

	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < newSetupIdx {
			m.cursor++
		}
	case "/":
		m.filtering = true
		m.filterInput.Focus()
	case "n":
		m.startWizard()
	case "d":
		if m.cursor < len(rows) {
			m.deleteTarget = m.cursor
		}
	case "enter":
		if m.cursor == newSetupIdx || len(rows) == 0 {
			m.startWizard()
			return m, nil
		}
		chosen := rows[m.cursor]
		m.profiles = touchProfile(m.profiles, chosen.Name, m.now)
		_ = saveProfiles(m.profilesPath, m.profiles)
		if m.outPath != "" {
			_ = writeOutFile(m.outPath, picksFromProfile(chosen))
		}
		m.launchProfile = &chosen
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *pickModel) startWizard() {
	m.mode = modeWizSub
	m.wizStep = 0
	m.wizPicks = [5]string{}
	m.subCursor = 0
	m.modelCursor = 0
	m.modelFilter.SetValue("")
	m.ensureCatalog()
}

func removeProfileByName(profiles []Profile, name string) []Profile {
	out := make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		if p.Name != name {
			out = append(out, p)
		}
	}
	return out
}

func (m pickModel) updateWizSub(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.subCursor > 0 {
			m.subCursor--
		}
	case "down", "j":
		if m.subCursor < len(m.catalog)-1 {
			m.subCursor++
		}
	case "s":
		m.wizPicks[m.wizStep] = ""
		return m.advanceWizStep()
	case "r":
		// DEFECT 1, round 2: bypass the 10-minute freshness window and
		// re-fetch this row's upstream(s) right now. No-op on a fixed-label
		// route (Upstreams empty -- only the caller holds that OAuth) and
		// while a refresh is already in flight.
		if m.refreshing || m.subCursor >= len(m.catalog) {
			return m, nil
		}
		opt := m.catalog[m.subCursor]
		if len(opt.Upstreams) == 0 {
			return m, nil
		}
		m.refreshing = true
		m.refreshTarget = opt.Name
		m.refreshErr = ""
		return m, tea.Batch(m.spin.Tick, refreshSubscriptionCmd(m.cfg, opt.Name, opt.Upstreams))
	case "esc":
		if m.wizStep > 0 {
			m.mode = modeWizModel
			m.wizStep--
			m.modelCursor = 0
			m.modelFilter.SetValue("")
		}
		// step 0: esc is a no-op, never leaves the wizard (§2.3).
	case "enter":
		if len(m.catalog) == 0 || m.subCursor >= len(m.catalog) {
			return m, nil
		}
		opt := m.catalog[m.subCursor]
		if opt.Unavailable != "" {
			return m, nil // not selectable, matches spec §2.4
		}
		m.mode = modeWizModel
		m.modelCursor = 0
		m.modelFilter.SetValue("")
		m.modelFilter.Focus()
	}
	return m, nil
}

func (m pickModel) currentModelIDs() []string {
	if m.subCursor >= len(m.catalog) {
		return nil
	}
	opt := m.catalog[m.subCursor]
	q := strings.ToLower(m.modelFilter.Value())
	if q == "" {
		return opt.ModelIDs
	}
	var out []string
	for _, id := range opt.ModelIDs {
		if strings.Contains(strings.ToLower(id), q) {
			out = append(out, id)
		}
	}
	return out
}

func (m pickModel) updateWizModel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeWizSub
		m.modelFilter.Blur()
		return m, nil
	case "up", "ctrl+p":
		if m.modelCursor > 0 {
			m.modelCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		ids := m.currentModelIDs()
		if m.modelCursor < len(ids)-1 {
			m.modelCursor++
		}
		return m, nil
	case "enter":
		ids := m.currentModelIDs()
		if m.modelCursor >= len(ids) {
			return m, nil
		}
		m.wizPicks[m.wizStep] = ids[m.modelCursor]
		return m.advanceWizStep()
	}
	var cmd tea.Cmd
	m.modelFilter, cmd = m.modelFilter.Update(msg)
	m.modelCursor = 0
	return m, cmd
}

// advanceWizStep moves to the next tier's subscription pane, or into the
// name prompt once haiku (the last tier) is set.
func (m pickModel) advanceWizStep() (tea.Model, tea.Cmd) {
	if m.wizStep >= len(tierKeys)-1 {
		m.mode = modeWizName
		m.nameInput.SetValue(autoName(m.wizPicks))
		m.nameInput.Focus()
		m.nameInput.CursorEnd()
		return m, nil
	}
	m.wizStep++
	m.mode = modeWizSub
	m.subCursor = 0
	m.modelCursor = 0
	m.modelFilter.SetValue("")
	m.modelFilter.Blur()
	return m, nil
}

func (m pickModel) updateWizName(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.wizStep = len(tierKeys) - 1
		m.mode = modeWizModel
		m.nameInput.Blur()
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			return m, nil
		}
		p := Profile{
			Name:   name,
			Main:   m.wizPicks[0],
			Fable:  m.wizPicks[1],
			Opus:   m.wizPicks[2],
			Sonnet: m.wizPicks[3],
			Haiku:  m.wizPicks[4],
		}
		m.profiles = removeProfileByName(m.profiles, name)
		m.profiles = append(m.profiles, p)
		m.profiles = touchProfile(m.profiles, name, m.now)
		sortProfiles(m.profiles)
		_ = saveProfiles(m.profilesPath, m.profiles)
		if m.outPath != "" {
			_ = writeOutFile(m.outPath, picksFromProfile(p))
		}
		launched := p
		m.launchProfile = &launched
		m.quitting = true
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func autoName(picks [5]string) string {
	seen := map[string]bool{}
	var parts []string
	for _, id := range picks {
		if id == "" {
			continue
		}
		tok := shortToken(id)
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		parts = append(parts, tok)
	}
	if len(parts) == 0 {
		return "setup"
	}
	return strings.Join(parts, "-")
}

func shortToken(id string) string {
	s := strings.ToLower(id)
	s = strings.TrimPrefix(s, "claude-")
	segs := strings.Split(s, "-")
	var out []string
	for _, seg := range segs {
		if seg == "" || isNumericish(seg) {
			continue
		}
		out = append(out, seg)
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return s
	}
	return strings.Join(out, "-")
}

func isNumericish(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return len(s) > 0
}

// ---- view -----------------------------------------------------------------

var (
	styleTitle   = lipgloss.NewStyle().Bold(true)
	styleRule    = lipgloss.NewStyle().Faint(true)
	styleSel     = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleWarn    = lipgloss.NewStyle().Bold(true)
	styleStatus  = lipgloss.NewStyle().Faint(true)
	styleUnavail = lipgloss.NewStyle().Faint(true).Strikethrough(false)
)

func hr(width int) string {
	if width <= 0 {
		width = 60
	}
	return styleRule.Render(strings.Repeat("─", width))
}

func (m pickModel) View() string {
	if m.quitting {
		return ""
	}
	w := m.width
	if w <= 0 {
		w = 72
	}
	var b strings.Builder
	fmt.Fprintln(&b, styleTitle.Render("submux pick"))
	fmt.Fprintln(&b, hr(w))
	if m.warning != "" {
		fmt.Fprintln(&b, styleWarn.Render("! "+m.warning))
	}

	switch m.mode {
	case modeHistory:
		b.WriteString(m.viewHistory())
	case modeWizSub:
		b.WriteString(m.viewWizSub())
	case modeWizModel:
		b.WriteString(m.viewWizModel())
	case modeWizName:
		b.WriteString(m.viewWizName())
	}

	fmt.Fprintln(&b, hr(w))
	fmt.Fprintln(&b, styleStatus.Render(m.statusBar()))
	return b.String()
}

func (m pickModel) statusBar() string {
	mode := "history"
	switch m.mode {
	case modeWizSub:
		mode = "create: choose subscription (" + tierLabels[m.wizStep] + ")"
	case modeWizModel:
		mode = "create: choose model (" + tierLabels[m.wizStep] + ")"
	case modeWizName:
		mode = "create: name and save"
	}
	model := "-"
	if m.mode == modeWizModel || m.mode == modeWizSub {
		if m.wizPicks[m.wizStep] != "" {
			model = m.wizPicks[m.wizStep]
		}
	}
	base := fmt.Sprintf("Mode: %s  Model: %s", mode, model)
	if m.refreshing {
		return base + "  " + m.spin.View() + " refreshing " + m.refreshTarget + "…"
	}
	if m.refreshErr != "" {
		return base + "  ! " + m.refreshErr
	}
	return base
}

func (m pickModel) viewHistory() string {
	var b strings.Builder
	if m.filtering {
		fmt.Fprintf(&b, "filter: %s\n", m.filterInput.View())
	}
	rows := m.visibleProfiles()
	if len(rows) == 0 && !m.filtering {
		fmt.Fprintln(&b, styleDim.Render("no saved setups yet"))
	}
	for i, p := range rows {
		line := fmt.Sprintf("%-24s  %3d use(s)  %s", p.Name, p.Uses, relativeTime(p.LastUsed, m.now))
		if i == m.cursor {
			fmt.Fprintln(&b, styleSel.Render("> "+line))
			b.WriteString(m.renderExpandedRow(p))
			if m.deleteTarget == i {
				fmt.Fprintln(&b, styleWarn.Render("    press d again to delete, any other key cancels"))
			}
		} else {
			fmt.Fprintln(&b, "  "+line)
		}
	}
	fmt.Fprintln(&b, hr(20))
	newLine := "+ create a new setup"
	if m.cursor == len(rows) {
		fmt.Fprintln(&b, styleSel.Render("> "+newLine))
	} else {
		fmt.Fprintln(&b, "  "+newLine)
	}
	fmt.Fprintln(&b, styleDim.Render("↑↓/jk move · enter select · n new · d delete · / filter · q quit"))
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// renderExpandedRow answers spec §2.2 "each with the subscription that pays
// for it" (DEFECT 2, round 2): one line per tier, id and payer in their own
// aligned columns instead of one cramped `key=id` line with no payer at
// all. An unset tier renders dim, distinct from a resolved-but-unknown
// payer. Column widths are computed from the actual ids/labels, never
// hardcoded (ids vary a lot in length).
func (m pickModel) renderExpandedRow(p Profile) string {
	type slot struct{ key, id string }
	slots := []slot{
		{"main", p.Main}, {"fable", p.Fable}, {"opus", p.Opus},
		{"sonnet", p.Sonnet}, {"haiku", p.Haiku},
	}
	labelW, idW := 0, 0
	for _, s := range slots {
		if len(s.key) > labelW {
			labelW = len(s.key)
		}
		if s.id != "" && len(s.id) > idW {
			idW = len(s.id)
		}
	}
	var b strings.Builder
	for _, s := range slots {
		if s.id == "" {
			fmt.Fprintf(&b, "    %-*s  %s\n", labelW, s.key, styleDim.Render("—  (session default)"))
			continue
		}
		payer := m.paidBy(s.id)
		if payer == "" {
			payer = styleDim.Render("(unresolved)")
		}
		fmt.Fprintf(&b, "    %-*s  %-*s  %s\n", labelW, s.key, idW, s.id, payer)
	}
	return b.String()
}

// paidBy resolves who pays for model id against the ALREADY-cached
// catalogue only -- it never fires a network call (spec: "NOT a fresh
// network call per keystroke"). If the wizard's catalog is already loaded
// in memory it reuses that; otherwise it falls back to a plain read of the
// on-disk models cache (no fetch, no write), which is stale-tolerant by
// design (spec §2.5).
func (m pickModel) paidBy(id string) string {
	entries := m.cacheEntries
	if entries == nil {
		entries = loadModelsCache(m.cachePath)
	}
	return payerForModelID(m.cfg, entries, id)
}

func (m pickModel) viewWizSub() string {
	var b strings.Builder
	fmt.Fprintf(&b, "step %d/%d -- pick a subscription for %s\n\n", m.wizStep+1, len(tierKeys), tierLabels[m.wizStep])
	if len(m.catalog) == 0 {
		fmt.Fprintln(&b, styleDim.Render("no subscriptions available"))
	}
	for i, opt := range m.catalog {
		label := fmt.Sprintf("%-28s  %d id(s)", opt.Name, len(opt.ModelIDs))
		if opt.Unavailable != "" {
			label = opt.Name + "  " + styleUnavail.Render("("+opt.Unavailable+")")
			if len(opt.Upstreams) > 0 {
				label += styleDim.Render(" · r to retry")
			}
		} else if opt.CacheAge != "" {
			label += styleDim.Render("  models cached " + opt.CacheAge + " · r to refresh")
		}
		if m.refreshing && opt.Name == m.refreshTarget {
			label += "  " + m.spin.View() + styleDim.Render("refreshing…")
		}
		if i == m.subCursor {
			fmt.Fprintln(&b, styleSel.Render("> "+label))
		} else {
			fmt.Fprintln(&b, "  "+label)
		}
	}
	fmt.Fprintln(&b, styleDim.Render("↑↓/jk move · enter choose · s skip tier · r refresh · esc back · q quit"))
	return b.String()
}

func (m pickModel) viewWizModel() string {
	var b strings.Builder
	sub := "?"
	if m.subCursor < len(m.catalog) {
		sub = m.catalog[m.subCursor].Name
	}
	fmt.Fprintf(&b, "step %d/%d -- pick a model in %s for %s\n", m.wizStep+1, len(tierKeys), sub, tierLabels[m.wizStep])
	fmt.Fprintf(&b, "filter: %s\n\n", m.modelFilter.View())
	ids := m.currentModelIDs()
	if len(ids) == 0 {
		fmt.Fprintln(&b, styleDim.Render("no models match"))
	}
	for i, id := range ids {
		if i == m.modelCursor {
			fmt.Fprintln(&b, styleSel.Render("> "+id))
		} else {
			fmt.Fprintln(&b, "  "+id)
		}
	}
	fmt.Fprintln(&b, styleDim.Render("↑↓ move · type to filter · enter choose · esc back · q quit"))
	return b.String()
}

func (m pickModel) viewWizName() string {
	var b strings.Builder
	fmt.Fprintln(&b, "name this setup:")
	fmt.Fprintf(&b, "> %s\n\n", m.nameInput.View())
	for i, k := range tierKeys {
		fmt.Fprintf(&b, "  %-6s %s\n", tierLabels[i]+":", orDash(m.wizPicks[i]))
		_ = k
	}
	fmt.Fprintln(&b, styleDim.Render("enter save & launch · esc back"))
	return b.String()
}
