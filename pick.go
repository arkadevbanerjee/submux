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
	"math"
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
	notice := fs.String("notice", "", "banner shown at the top, e.g. why the previous pick was refused")
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
	if *notice != "" {
		if warning != "" {
			warning = *notice + " · " + warning
		} else {
			warning = *notice
		}
	}

	tty, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if ttyErr != nil {
		// No controlling terminal (spec §3): degrade to a plain numbered
		// list read from stdin, never hang, never touch the alt-screen.
		os.Exit(runPlainPicker(cfg, pPath, profiles, *outPath))
	}
	defer func() { _ = tty.Close() }()

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
	_, _ = fmt.Sscanf(line, "%d", &idx) // non-numeric input leaves idx at -1
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
	Effort                           string
	ProfileName                      string
}

func picksFromProfile(p Profile) picks {
	return picks{Main: p.Main, Fable: p.Fable, Opus: p.Opus, Sonnet: p.Sonnet, Haiku: p.Haiku, Effort: p.Effort, ProfileName: p.Name}
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
	writeIfSet("SUBMUX_EFFORT", p.Effort)
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
	modeWizEffort
	modeWizName
	modeAlias       // 'a' in history: edit the alias of one saved setup
	modeWizFallback // Enter on a dead subscription row: ask before pointing at its free fallback
)

var effortOptions = [6]string{"", "low", "medium", "high", "xhigh", "max"}

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
	probes       map[string]probeResult // subscription -> latest probe, re-applied after every catalog rebuild
	probesSent   bool                   // ONE background probe batch per picker session

	modelProbes   map[string]map[string]probeResult // subscription -> model id -> per-model probe
	modelProbedAt map[string]time.Time              // subscription -> when its models were last probed
	aliasTarget   string                            // profile Name being aliased in modeAlias
	notice        string                            // one-line reason shown under the list (why Enter did nothing)
	fbTo          int                               // catalog index of the fallback offered in modeWizFallback

	burn               burnFile // loaded from disk by ensureCatalog, merged by burnRefreshMsg (§S8)
	burnRefreshChecked bool     // ONE background burn refresh per picker session, never blocking
	burnRefreshErr     string   // last burn refresh failure, surfaced in the status bar

	spin          spinner.Model
	refreshing    bool   // a manual 'r' refresh (DEFECT 1, round 2) is in flight
	refreshTarget string // subscription Name being refreshed
	refreshErr    string // last refresh failure, surfaced in the status bar until the next refresh
	refreshNote   string // last refresh's outcome ("refreshed X: n ids, +a new, -b gone"), so an unchanged list still shows the key worked

	wizStep      int
	wizPicks     [5]string
	effortCursor int
	wizEffort    string
	subCursor    int
	modelCursor  int
	modelFilter  textinput.Model
	nameInput    textinput.Model

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
		if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.Alias), q) {
			out = append(out, p)
		}
	}
	return out
}

// ensureCatalog lazily builds the subscription catalog and loads the burn
// cache (§S8). Both loads are disk-only: no network, no blocking -- a stale
// or missing burn.json still renders (missing rows show not-disclosed). When
// either half of the burn cache is past its freshness window it returns ONE
// background refresh cmd (once per picker session); the caller must hand it
// back to the runtime, mirroring how the 'r' refresh dispatches.
func (m *pickModel) ensureCatalog() tea.Cmd {
	var cmd tea.Cmd
	if m.catalog == nil {
		entries := loadModelsCache(m.cachePath)
		m.catalog = buildSubscriptionCatalog(m.cfg, entries, m.now)
		applyProbes(m.cfg, m.catalog, m.probes)
		m.cacheEntries = entries
		if err := saveModelsCache(m.cachePath, entries); err != nil {
			m.catalogWarn = fmt.Sprintf("could not write models cache: %v", err)
		}
	}
	if m.burn.Providers == nil {
		if p, err := burnPath(); err == nil {
			m.burn = loadBurnFile(p)
		}
	}
	if !m.burnRefreshChecked {
		m.burnRefreshChecked = true
		providers, measured := m.burnNeedsRefresh()
		if providers || measured {
			cmd = refreshBurnCmd(m.cfg, m.cacheEntries, m.burn, providers, measured)
		}
	}
	if !m.probesSent {
		m.probesSent = true
		cmd = tea.Batch(cmd, probeCmd(m.cfg))
	}
	return cmd
}

// burnNeedsRefresh reports which halves of the burn cache are past their
// freshness window (7d provider, 24h measured, §S8). A cold file counts as
// stale: the very first picker session populates it in the background.
func (m pickModel) burnNeedsRefresh() (providers, measured bool) {
	now := time.Now()
	providers = m.burn.ProviderFetchedAt.IsZero() || now.Sub(m.burn.ProviderFetchedAt) > burnProviderFreshFor
	measured = m.burn.MeasuredFetchedAt.IsZero() || now.Sub(m.burn.MeasuredFetchedAt) > burnMeasuredFreshFor
	return providers, measured
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
	case probeResultsMsg:
		if m.probes == nil {
			m.probes = map[string]probeResult{}
		}
		for _, r := range msg.results {
			m.probes[r.Subscription] = r
		}
		if m.catalog != nil {
			m.catalog = buildSubscriptionCatalog(m.cfg, m.cacheEntries, time.Now())
			applyProbes(m.cfg, m.catalog, m.probes)
		}
		return m, nil
	case modelProbeMsg:
		if m.modelProbes == nil {
			m.modelProbes = map[string]map[string]probeResult{}
		}
		m.modelProbes[msg.sub] = msg.results
		return m, nil
	case burnRefreshMsg:
		// §S8: msg.burn always started from the previous file, so even a
		// failed refresh keeps the stale data already on screen; the error
		// is surfaced in the status bar until the next refresh.
		m.burn = msg.burn
		m.burnRefreshErr = msg.err
		return m, nil
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
	added, gone := 0, 0
	for up, entry := range msg.upstreams {
		added, gone = added+countNewIDs(entry.Models, m.cacheEntries[up].Models), gone+countNewIDs(m.cacheEntries[up].Models, entry.Models)
		m.cacheEntries[up] = entry
	}
	if len(msg.upstreams) > 0 {
		if err := saveModelsCache(m.cachePath, m.cacheEntries); err != nil {
			m.catalogWarn = fmt.Sprintf("could not write models cache: %v", err)
		}
		m.catalog = buildSubscriptionCatalog(m.cfg, m.cacheEntries, time.Now())
		applyProbes(m.cfg, m.catalog, m.probes)
	}
	if len(msg.failedUpstreams) > 0 {
		errText := "refresh failed"
		if msg.firstErr != nil {
			errText = msg.firstErr.Error()
		}
		m.refreshErr = fmt.Sprintf("refresh of %s failed (%s) -- showing cached data", msg.subName, errText)
	} else {
		m.refreshErr = ""
		m.refreshNote = fmt.Sprintf("refreshed %s at %s: +%d new, -%d gone upstream", msg.subName, time.Now().Format("15:04:05"), added, gone)
	}
	for i, opt := range m.catalog {
		if opt.Name == msg.subName {
			m.subCursor = i
			break
		}
	}
	return m
}

// countNewIDs counts ids in next that are absent from prev.
func countNewIDs(next, prev []cachedModel) int {
	had := make(map[string]bool, len(prev))
	for _, p := range prev {
		had[p.ID] = true
	}
	n := 0
	for _, x := range next {
		if !had[x.ID] {
			n++
		}
	}
	return n
}

// startRefresh re-fetches the upstream(s) behind the highlighted subscription
// (the 'r' key on the subscription pane, ctrl+r on the model pane). It returns
// the model unchanged and a nil cmd when there is nothing to refresh or a
// refresh is already in flight.
func (m pickModel) startRefresh() (pickModel, tea.Cmd) {
	if m.refreshing || m.subCursor >= len(m.catalog) {
		return m, nil
	}
	opt := m.catalog[m.subCursor]
	if len(opt.Upstreams) == 0 {
		return m, nil
	}
	delete(m.modelProbes, opt.Name) // a renewed login must re-check its models too
	delete(m.modelProbedAt, opt.Name)
	m.refreshing = true
	m.refreshTarget = opt.Name
	m.refreshErr = ""
	m.refreshNote = ""
	// Re-probe too: a renewed plan should come back without restarting the picker.
	return m, tea.Batch(m.spin.Tick, refreshSubscriptionCmd(m.cfg, opt.Name, opt.Upstreams), probeCmd(m.cfg, opt.Name))
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
		burnCmd := m.ensureCatalog()
		next, cmd := m.updateWizSub(msg)
		if burnCmd != nil {
			return next, tea.Batch(burnCmd, cmd)
		}
		return next, cmd
	case modeWizModel:
		return m.updateWizModel(msg)
	case modeWizEffort:
		return m.updateWizEffort(msg)
	case modeWizName:
		return m.updateWizName(msg)
	case modeWizFallback:
		return m.updateWizFallback(msg)
	case modeAlias:
		return m.updateAlias(msg)
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
		cmd := m.startWizard()
		return m, cmd
	case "d":
		if m.cursor < len(rows) {
			m.deleteTarget = m.cursor
		}
	case "a":
		if m.cursor < len(rows) {
			m.aliasTarget = rows[m.cursor].Name
			m.mode = modeAlias
			m.notice = ""
			m.nameInput.Placeholder = "alias (empty clears)"
			m.nameInput.SetValue(rows[m.cursor].Alias)
			m.nameInput.Focus()
		}
	case "enter":
		if m.cursor == newSetupIdx || len(rows) == 0 {
			cmd := m.startWizard()
			return m, cmd
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

func (m *pickModel) startWizard() tea.Cmd {
	m.mode = modeWizSub
	m.wizStep = 0
	m.wizPicks = [5]string{}
	m.subCursor = 0
	m.modelCursor = 0
	m.modelFilter.SetValue("")
	return m.ensureCatalog()
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
		return m.startRefresh()
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
		m.notice = ""
		if opt.Unavailable != "" {
			// Never selectable (spec §2.4), and never swapped silently: offer the
			// configured free fallback and let the user say yes.
			fb := fallbackFor(m.cfg, opt.Name)
			for i, c := range m.catalog {
				if fb != "" && c.Name == fb && c.Unavailable == "" {
					m.fbTo = i
					m.mode = modeWizFallback
					return m, nil
				}
			}
			m.notice = opt.Name + " is unavailable and has no free fallback configured"
			return m, nil
		}
		return m.openModelPane()
	}
	return m, nil
}

// openModelPane enters the model list for the subscription under subCursor and
// starts a per-model probe (skipped while a recent one is still fresh).
func (m pickModel) openModelPane() (tea.Model, tea.Cmd) {
	m.mode = modeWizModel
	m.modelCursor = 0
	m.modelFilter.SetValue("")
	m.modelFilter.Focus()
	opt := m.catalog[m.subCursor]
	if at, ok := m.modelProbedAt[opt.Name]; ok && time.Since(at) < modelProbeFreshFor {
		return m, nil
	}
	if m.modelProbedAt == nil {
		m.modelProbedAt = map[string]time.Time{}
	}
	if skipModelProbe(m.cfg, opt.Name) {
		return m, nil
	}
	m.modelProbedAt[opt.Name] = time.Now()
	return m, probeModelsCmd(m.cfg, opt.Name, opt.ModelIDs)
}

// updateWizFallback answers "use <free fallback> instead?": y goes to the
// fallback's model list (the user still picks the model), anything else returns.
func (m pickModel) updateWizFallback(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.subCursor = m.fbTo
		m.notice = ""
		return m.openModelPane()
	case "n", "N", "esc", "q", "enter":
		m.mode = modeWizSub
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
		m.notice = ""
		m.modelFilter.Blur()
		return m, nil
	case "ctrl+r":
		// Plain 'r' here is a filter character, so refresh gets its own chord.
		next, cmd := m.startRefresh()
		next.modelCursor = 0
		return next, cmd
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
		if m.subCursor < len(m.catalog) {
			r, ok := m.modelProbes[m.catalog[m.subCursor].Name][ids[m.modelCursor]]
			if why, blocked := modelBlocked(r, ok); blocked {
				m.notice = ids[m.modelCursor] + " cannot answer: " + why
				return m, nil
			}
		}
		m.notice = ""
		m.wizPicks[m.wizStep] = ids[m.modelCursor]
		return m.advanceWizStep()
	}
	var cmd tea.Cmd
	m.modelFilter, cmd = m.modelFilter.Update(msg)
	m.modelCursor = 0
	return m, cmd
}

// advanceWizStep moves to the next tier's subscription pane, or into the
// effort prompt once haiku (the last tier) is set.
func (m pickModel) advanceWizStep() (tea.Model, tea.Cmd) {
	if m.wizStep >= len(tierKeys)-1 {
		m.mode = modeWizEffort
		m.effortCursor = 0
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

func (m pickModel) updateWizEffort(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "esc":
		m.mode = modeWizModel
		m.wizStep = len(tierKeys) - 1
		return m, nil
	case "up", "ctrl+p":
		if m.effortCursor > 0 {
			m.effortCursor--
		}
	case "down", "ctrl+n":
		if m.effortCursor < len(effortOptions)-1 {
			m.effortCursor++
		}
	case "enter":
		m.wizEffort = effortOptions[m.effortCursor]
		m.mode = modeWizName
		m.nameInput.SetValue(autoName(m.wizPicks))
		m.nameInput.Focus()
		m.nameInput.CursorEnd()
		return m, nil
	}
	return m, nil
}

func (m pickModel) updateWizName(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeWizEffort
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
			Effort: m.wizEffort,
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
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:] // drop the provider prefix ("kiro/", "zen/", "ogo/")
	}
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
	case modeWizEffort:
		b.WriteString(m.viewWizEffort())
	case modeWizName:
		b.WriteString(m.viewWizName())
	case modeWizFallback:
		b.WriteString(m.viewWizFallback())
	case modeAlias:
		b.WriteString(m.viewAlias())
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
	case modeWizEffort:
		mode = "create: choose effort"
	case modeWizName:
		mode = "create: name and save"
	case modeWizFallback:
		mode = "create: confirm free fallback"
	case modeAlias:
		mode = "set alias"
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
	if m.refreshNote != "" {
		return base + "  " + m.refreshNote
	}
	if m.burnRefreshErr != "" {
		return base + "  ! burn refresh failed -- showing cached burn data (" + m.burnRefreshErr + ")"
	}
	if age := m.burn.oldestDataAge(time.Now()); age > burnStaleWarnAfter {
		return base + fmt.Sprintf("  ! burn data %dd old", int(age.Hours()/24))
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
		label := p.Name
		if p.Alias != "" {
			label = p.Alias + "  (" + p.Name + ")"
		}
		line := fmt.Sprintf("%-24s  %3d use(s)  %s", label, p.Uses, relativeTime(p.LastUsed, m.now))
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
	fmt.Fprintln(&b, styleDim.Render("↑↓/jk move · enter select · n new · a alias · d delete · / filter · q quit"))
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
			fmt.Fprintf(&b, "    %-*s  %-*s  %s\n", labelW, s.key, idW, s.id, styleDim.Render("(unresolved)"))
			continue
		}
		// §S7: third column -- burn indicator keyed by the resolved payer.
		fmt.Fprintf(&b, "    %-*s  %-*s  %s  %s\n", labelW, s.key, idW, s.id, payer, m.burnCell(payer, s.id))
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

// burnProviders returns the on-hand provider -> model -> entry map, reading
// the disk cache directly when the wizard has not run yet this session --
// the same stale-tolerant, no-fetch fallback paidBy uses, so history-mode
// expanded rows still show burn data without ever entering the wizard.
func (m pickModel) burnProviders() map[string]map[string]burnEntry {
	if m.burn.Providers != nil {
		return m.burn.Providers
	}
	p, err := burnPath()
	if err != nil {
		return nil
	}
	return loadBurnFile(p).Providers
}

// burnBarLen scales a weight to bar glyphs: log2 growth from one glyph at
// 1x, capped at eight (§S7).
func burnBarLen(w float64) int {
	if w <= 1 {
		return 1
	}
	n := int(math.Log2(w)) + 1
	if n > 8 {
		n = 8
	}
	return n
}

// formatBurnWeight prints a burn multiplier at a readable precision: one
// decimal below 10x (trailing ".0" dropped), whole numbers from 10x up, so a
// raw ratio like 1.344887382 shows as "1.3" and 2746.4001588 as "2746".
func formatBurnWeight(w float64) string {
	if w >= 10 {
		return fmt.Sprintf("%.0f", w)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", w), ".0")
}

// burnCell renders the bar + relative burn multiplier for one model id
// under one provider (§S7). Weights are per-provider relative -- each
// provider's lightest model is that screen's 1x, never one global scale. A
// missing or Source:"none" entry renders the dim not-disclosed form, NEVER
// "1x" (runbook hard rule). A measured row is labelled so it is never
// mistaken for a provider figure.
func (m pickModel) burnCell(provider, id string) string {
	e, ok := m.burnProviders()[provider][id]
	// A measured row is a ccusage token ratio, not a cost multiplier (it
	// produced "~13063x"), so it renders as not disclosed like a missing one.
	// Its hardcoded note (weekly cap, effort ignored) still applies.
	if !ok || e.Source == "none" || e.Source == "measured" || e.Weight <= 0 {
		s := styleDim.Render("—  (not disclosed)")
		if ok && e.Note != "" {
			s += " " + e.Note
		}
		return s
	}
	bar := strings.Repeat("▇", burnBarLen(e.Weight))
	s := fmt.Sprintf("%s  ~%sx", bar, formatBurnWeight(e.Weight))
	if e.Note != "" {
		s += " " + e.Note
	}
	return s
}

func (m pickModel) viewWizSub() string {
	var b strings.Builder
	fmt.Fprintf(&b, "step %d/%d -- pick a subscription for %s\n\n", m.wizStep+1, len(tierKeys), tierLabels[m.wizStep])
	if len(m.catalog) == 0 {
		fmt.Fprintln(&b, styleDim.Render("no subscriptions available"))
	}
	nameW := 28
	for _, opt := range m.catalog {
		if w := len([]rune(opt.Name)); w > nameW {
			nameW = w
		}
	}
	for i, opt := range m.catalog {
		label := fmt.Sprintf("%-*s  %d id(s)", nameW, opt.Name, len(opt.ModelIDs))
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
	if m.notice != "" {
		fmt.Fprintln(&b, styleWarn.Render("! "+m.notice))
	}
	fmt.Fprintln(&b, styleDim.Render("↑↓/jk move · enter choose · s skip tier · r refresh · esc back · q quit"))
	return b.String()
}

// viewWizFallback is the explicit y/n before the free fallback is offered.
func (m pickModel) viewWizFallback() string {
	var b strings.Builder
	from := m.catalog[m.subCursor]
	to := m.catalog[m.fbTo]
	fmt.Fprintf(&b, "%s\n  %s\n\n", from.Name, styleUnavail.Render(from.Unavailable))
	fmt.Fprintf(&b, "Use %s instead?  (y = open its models, n = back)\n", styleSel.Render(" "+to.Name+" "))
	fmt.Fprintln(&b, styleDim.Render("y yes · n/esc back · q back"))
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
	// §S7: pad the id column the way renderExpandedRow does, then append the
	// burn cell for this subscription.
	idW := 0
	for _, id := range ids {
		if len(id) > idW {
			idW = len(id)
		}
	}
	for i, id := range ids {
		line := fmt.Sprintf("%-*s  %s", idW, id, m.burnCell(sub, id))
		r, probed := m.modelProbes[sub][id]
		if why, blocked := modelBlocked(r, probed); blocked {
			line = fmt.Sprintf("%-*s  ", idW, id) + styleUnavail.Render("("+why+")")
		} else if !probed && m.modelProbesPending(sub) {
			line += styleDim.Render("  checking…")
		}
		if i == m.modelCursor {
			fmt.Fprintln(&b, styleSel.Render("> "+line))
		} else {
			fmt.Fprintln(&b, "  "+line)
		}
	}
	if m.notice != "" {
		fmt.Fprintln(&b, styleWarn.Render("! "+m.notice))
	}
	fmt.Fprintln(&b, styleDim.Render("↑↓ move · type to filter · ctrl+r refresh · enter choose · esc back · q quit"))
	return b.String()
}

func (m pickModel) viewWizEffort() string {
	var b strings.Builder
	fmt.Fprintln(&b, "effort -- applies to the whole session (main loop and all four subagent tiers)")
	fmt.Fprintln(&b)
	for i, k := range tierKeys {
		row := orDash(m.wizPicks[i])
		if k == "haiku" {
			row += "  " + styleDim.Render("(haiku tier ignores effort)")
		}
		fmt.Fprintf(&b, "  %-6s %s\n", tierLabels[i]+":", row)
	}
	fmt.Fprintln(&b)
	for i, opt := range effortOptions {
		label := opt
		if label == "" {
			label = "(unset)  — keep Claude Code's own default"
		}
		if i == m.effortCursor {
			fmt.Fprintln(&b, styleSel.Render("> "+label))
		} else {
			fmt.Fprintln(&b, "  "+label)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleDim.Render("↑↓ move · enter choose · esc back · q quit"))
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
	fmt.Fprintf(&b, "  %-6s %s\n", "effort:", orDash(m.wizEffort))
	fmt.Fprintln(&b, styleDim.Render("enter save & launch · esc back"))
	return b.String()
}

// modelProbesPending reports whether sub's per-model probe batch is still in flight.
func (m pickModel) modelProbesPending(sub string) bool {
	_, started := m.modelProbedAt[sub]
	_, done := m.modelProbes[sub]
	return started && !done
}

// updateAlias edits one setup's alias: enter saves (empty clears), esc cancels.
// An alias must be one word and unique, or `sc <alias>` would be ambiguous.
func (m pickModel) updateAlias(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeHistory
		m.nameInput.Blur()
		return m, nil
	case "enter":
		alias := strings.TrimSpace(m.nameInput.Value())
		if strings.ContainsAny(alias, " \t") {
			m.notice = "an alias is one word"
			return m, nil
		}
		for _, p := range m.profiles {
			if alias != "" && p.Name != m.aliasTarget && (strings.EqualFold(p.Alias, alias) || strings.EqualFold(p.Name, alias)) {
				m.notice = "'" + alias + "' is already used by " + p.Name
				return m, nil
			}
		}
		for i := range m.profiles {
			if m.profiles[i].Name == m.aliasTarget {
				m.profiles[i].Alias = alias
			}
		}
		_ = saveProfiles(m.profilesPath, m.profiles)
		m.mode = modeHistory
		m.notice = ""
		m.nameInput.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m pickModel) viewAlias() string {
	var b strings.Builder
	fmt.Fprintf(&b, "alias for %s\n\n  %s\n\n", m.aliasTarget, m.nameInput.View())
	if m.notice != "" {
		fmt.Fprintln(&b, styleWarn.Render("! "+m.notice))
	}
	fmt.Fprintln(&b, styleDim.Render("enter save · empty clears · esc cancel · then run: sc <alias>"))
	return b.String()
}
