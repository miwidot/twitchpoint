package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/miwi/twitchpoint/internal/drops"
	"github.com/miwi/twitchpoint/internal/farmer"
)

// tickMsg is sent periodically to refresh the UI.
type tickMsg time.Time

// tabID identifies the top-level tab the user is currently viewing.
type tabID int

const (
	tabChannels tabID = iota
	tabDrops
	tabHelp
)

// Model is the Bubbletea app model.
type Model struct {
	farmer *farmer.Farmer
	width  int
	height int

	// Active tab — switched via 1/2/3 number keys or Tab/Shift-Tab.
	activeTab tabID

	// Channel table scroll (tab 1).
	channelScroll int

	// Drops tab cursor state. focusedPanel selects which of the three
	// stacked panels (campaigns / wanted-games / settings) currently
	// receives j/k navigation. The per-panel cursors track row position
	// inside that panel.
	dropsFocusedPanel   dropsPanel
	dropsCampaignCursor int
	dropsGameCursor     int
	dropsSettingsCursor int

	// Input mode (text-input modals — channel add/remove/priority + game
	// name prompt). Drops-tab inline interaction does NOT use this; only
	// the channels-tab text-input flows still go through it.
	inputMode  inputState
	inputValue string


	// Error display
	errMsg    string
	errExpiry time.Time

	// OnQuit is called when the user presses 'q'. If set, the TUI stays
	// running instead of exiting (used on Windows to hide the console).
	OnQuit func()

	quitting bool
}

// dropsPanel identifies which of the drops-tab sub-panels has the cursor.
type dropsPanel int

const (
	dropsPanelCampaigns dropsPanel = iota
	dropsPanelGames
	dropsPanelSettings
)

// inputState tracks which (if any) text-input modal is currently
// capturing keypresses. Channels tab uses the channel/priority modals;
// Drops tab uses inputAddGameName when the user presses '+' on the
// Wanted Games panel. The old inputGameList (modal-style game editor)
// and inputToggleCampaign (text-typed campaign-name toggle) are gone —
// the Drops tab handles both inline.
type inputState int

const (
	inputNone inputState = iota
	inputAddChannel
	inputRemoveChannel
	inputSetPriority
	inputAddGameName
)

// NewModel creates a new UI model.
func NewModel(f *farmer.Farmer) Model {
	return Model{
		farmer: f,
		width:  120,
		height: 40,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(),
		tea.WindowSize(),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		return m, tickCmd()
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// If a text-input modal is active (channel add/remove/priority or
	// game-name prompt from Drops tab), capture the keys.
	if m.inputMode != inputNone {
		switch msg.Type {
		case tea.KeyEnter:
			return m.submitInput()
		case tea.KeyEscape:
			m.inputMode = inputNone
			m.inputValue = ""
			return m, nil
		case tea.KeyBackspace:
			if len(m.inputValue) > 0 {
				m.inputValue = m.inputValue[:len(m.inputValue)-1]
			}
			return m, nil
		default:
			if msg.Type == tea.KeyRunes {
				m.inputValue += string(msg.Runes)
			}
			return m, nil
		}
	}

	// Tab navigation works regardless of which tab is currently active.
	switch msg.String() {
	case "q", "ctrl+c":
		if m.OnQuit != nil {
			m.OnQuit()
			return m, nil
		}
		m.quitting = true
		m.farmer.Stop()
		return m, tea.Quit
	case "1":
		m.activeTab = tabChannels
		return m, nil
	case "2":
		m.activeTab = tabDrops
		return m, nil
	case "3":
		m.activeTab = tabHelp
		return m, nil
	case "tab":
		m.activeTab = (m.activeTab + 1) % 3
		return m, nil
	case "shift+tab":
		m.activeTab = (m.activeTab + 2) % 3
		return m, nil
	}

	// Per-tab key handling.
	switch m.activeTab {
	case tabChannels:
		return m.handleChannelsKey(msg)
	case tabDrops:
		return m.handleDropsKey(msg)
	case tabHelp:
		// No interactive keys yet — Help tab is read-only.
		return m, nil
	}
	return m, nil
}

// handleChannelsKey dispatches keys for the Channels tab — text-input
// modal triggers (a/d/p) and channel-table scroll (j/k/home/end). The
// old 'g' (wanted-games modal) and 't' (toggle campaign modal) keys
// are gone — both flows live in the Drops tab now as inline panels.
func (m Model) handleChannelsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "a":
		m.inputMode = inputAddChannel
		m.inputValue = ""
		return m, nil
	case "d":
		m.inputMode = inputRemoveChannel
		m.inputValue = ""
		return m, nil
	case "p":
		m.inputMode = inputSetPriority
		m.inputValue = ""
		return m, nil
	case "up", "k":
		if m.channelScroll > 0 {
			m.channelScroll--
		}
		return m, nil
	case "down", "j":
		m.channelScroll++
		return m, nil
	case "home":
		m.channelScroll = 0
		return m, nil
	case "end":
		m.channelScroll = 9999 // clamped in View
		return m, nil
	}
	return m, nil
}

// handleDropsKey dispatches keys for the Drops tab. The cursor is
// unified across the three stacked panels (Drop Campaigns, Wanted
// Games, Settings) — j/k overflows panel boundaries so the user
// navigates the whole tab body as one continuous list. Per-panel
// action keys (Space, +, -, u, d) operate on the row currently under
// the cursor and only do something meaningful when the focused panel
// supports that action.
//
// If the user presses '+' to add a game, we delegate to the legacy
// inputAddGameName modal — the only text-input flow Drops tab needs
// (everything else is selection or boolean toggle).
func (m Model) handleDropsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	drops := m.farmer.GetActiveDrops()
	games := m.farmer.Config().GetGamesToWatch()
	settings := dropsSettings(m.farmer.Config())

	switch msg.String() {
	case "down", "j":
		m = m.dropsCursorDown(len(drops), len(games), len(settings))
		return m, nil
	case "up", "k":
		m = m.dropsCursorUp(len(drops), len(games), len(settings))
		return m, nil
	case " ", "space":
		return m.dropsToggle(drops, settings), nil

	// '+' / '-' / 'u' / 'd' are wanted-games actions. They auto-focus
	// the Games panel so the user doesn't have to navigate j/k there
	// first — this is the most common Drops-tab interaction and
	// requiring a precondition (cursor on Games panel) was confusing.
	case "+":
		m.dropsFocusedPanel = dropsPanelGames
		m.inputMode = inputAddGameName
		m.inputValue = ""
		return m, nil
	case "-":
		m.dropsFocusedPanel = dropsPanelGames
		if m.dropsGameCursor < len(games) {
			m.farmer.Config().RemoveGameFromWatch(games[m.dropsGameCursor])
			_ = m.farmer.Config().Save()
			if m.dropsGameCursor > 0 && m.dropsGameCursor >= len(games)-1 {
				m.dropsGameCursor--
			}
		}
		return m, nil
	case "u":
		m.dropsFocusedPanel = dropsPanelGames
		if m.dropsGameCursor > 0 && m.dropsGameCursor < len(games) {
			m.farmer.Config().MoveGameToWatch(games[m.dropsGameCursor], -1)
			_ = m.farmer.Config().Save()
			m.dropsGameCursor--
		}
		return m, nil
	case "d":
		m.dropsFocusedPanel = dropsPanelGames
		if m.dropsGameCursor < len(games)-1 {
			m.farmer.Config().MoveGameToWatch(games[m.dropsGameCursor], +1)
			_ = m.farmer.Config().Save()
			m.dropsGameCursor++
		}
		return m, nil
	}
	return m, nil
}

// dropsCursorDown advances the unified cursor by one row, overflowing
// to the next panel when the current panel's last row is already
// active. Wraps around from Settings → Campaigns.
func (m Model) dropsCursorDown(nCampaigns, nGames, nSettings int) Model {
	switch m.dropsFocusedPanel {
	case dropsPanelCampaigns:
		if m.dropsCampaignCursor < nCampaigns-1 {
			m.dropsCampaignCursor++
		} else if nGames > 0 {
			m.dropsFocusedPanel = dropsPanelGames
			m.dropsGameCursor = 0
		} else if nSettings > 0 {
			m.dropsFocusedPanel = dropsPanelSettings
			m.dropsSettingsCursor = 0
		}
	case dropsPanelGames:
		if m.dropsGameCursor < nGames-1 {
			m.dropsGameCursor++
		} else if nSettings > 0 {
			m.dropsFocusedPanel = dropsPanelSettings
			m.dropsSettingsCursor = 0
		}
	case dropsPanelSettings:
		if m.dropsSettingsCursor < nSettings-1 {
			m.dropsSettingsCursor++
		} else if nCampaigns > 0 {
			m.dropsFocusedPanel = dropsPanelCampaigns
			m.dropsCampaignCursor = 0
		}
	}
	return m
}

// dropsCursorUp is the inverse — moves up, overflowing to the previous
// panel's last row. Wraps from Campaigns → Settings.
func (m Model) dropsCursorUp(nCampaigns, nGames, nSettings int) Model {
	switch m.dropsFocusedPanel {
	case dropsPanelCampaigns:
		if m.dropsCampaignCursor > 0 {
			m.dropsCampaignCursor--
		} else if nSettings > 0 {
			m.dropsFocusedPanel = dropsPanelSettings
			m.dropsSettingsCursor = nSettings - 1
		} else if nGames > 0 {
			m.dropsFocusedPanel = dropsPanelGames
			m.dropsGameCursor = nGames - 1
		}
	case dropsPanelGames:
		if m.dropsGameCursor > 0 {
			m.dropsGameCursor--
		} else if nCampaigns > 0 {
			m.dropsFocusedPanel = dropsPanelCampaigns
			m.dropsCampaignCursor = nCampaigns - 1
		}
	case dropsPanelSettings:
		if m.dropsSettingsCursor > 0 {
			m.dropsSettingsCursor--
		} else if nGames > 0 {
			m.dropsFocusedPanel = dropsPanelGames
			m.dropsGameCursor = nGames - 1
		} else if nCampaigns > 0 {
			m.dropsFocusedPanel = dropsPanelCampaigns
			m.dropsCampaignCursor = nCampaigns - 1
		}
	}
	return m
}

// dropsToggle handles the Space key. In the Campaigns panel it flips
// the campaign's enabled/disabled state. In the Settings panel it
// toggles the boolean setting. In the Games panel Space is a no-op
// (use +/-/u/d there).
func (m Model) dropsToggle(drops []drops.ActiveDrop, settings []dropsSetting) Model {
	switch m.dropsFocusedPanel {
	case dropsPanelCampaigns:
		if m.dropsCampaignCursor < len(drops) {
			d := drops[m.dropsCampaignCursor]
			// Compute new state: COMPLETED rows stay completed (toggle no-op),
			// DISABLED → enabled, anything else → disabled.
			newEnabled := !d.IsEnabled
			switch d.Status {
			case "DISABLED":
				newEnabled = true
			case "ACTIVE", "QUEUED", "IDLE":
				newEnabled = false
			case "COMPLETED":
				m.errMsg = "campaign already COMPLETED — cannot toggle"
				m.errExpiry = time.Now().Add(3 * time.Second)
				return m
			}
			if err := m.farmer.SetCampaignEnabled(d.CampaignID, newEnabled); err != nil {
				m.errMsg = fmt.Sprintf("Error: %v", err)
				m.errExpiry = time.Now().Add(5 * time.Second)
			} else {
				word := "disabled"
				if newEnabled {
					word = "enabled"
				}
				m.errMsg = fmt.Sprintf("%s %q", word, d.CampaignName)
				m.errExpiry = time.Now().Add(3 * time.Second)
			}
		}
	case dropsPanelSettings:
		if m.dropsSettingsCursor < len(settings) {
			s := settings[m.dropsSettingsCursor]
			s.toggle(m.farmer.Config())
			_ = m.farmer.Config().Save()
			m.errMsg = fmt.Sprintf("%s — restart required", s.label)
			m.errExpiry = time.Now().Add(3 * time.Second)
		}
	}
	return m
}

func (m Model) submitInput() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(strings.ToLower(m.inputValue))
	m.inputValue = ""

	switch m.inputMode {
	case inputAddChannel:
		if value != "" {
			if err := m.farmer.AddChannelLive(value); err != nil {
				m.errMsg = fmt.Sprintf("Error: %v", err)
				m.errExpiry = time.Now().Add(5 * time.Second)
			}
		}
	case inputRemoveChannel:
		if value != "" {
			if err := m.farmer.RemoveChannelLive(value); err != nil {
				m.errMsg = fmt.Sprintf("Error: %v", err)
				m.errExpiry = time.Now().Add(5 * time.Second)
			}
		}
	case inputSetPriority:
		// Format: "channelname 1" or "channelname 2"
		if value != "" {
			parts := strings.Fields(value)
			if len(parts) != 2 || (parts[1] != "1" && parts[1] != "2") {
				m.errMsg = "Format: channelname 1 (priority) or channelname 2 (rotate)"
				m.errExpiry = time.Now().Add(5 * time.Second)
			} else {
				pri := 1
				if parts[1] == "2" {
					pri = 2
				}
				if err := m.farmer.SetPriorityLive(parts[0], pri); err != nil {
					m.errMsg = fmt.Sprintf("Error: %v", err)
					m.errExpiry = time.Now().Add(5 * time.Second)
				}
			}
		}
	case inputAddGameName:
		// User typed a game name on the Wanted Games panel. Persist it
		// and jump the cursor to the freshly-added entry (which always
		// goes to the end of the list) so the user can immediately
		// reorder / delete it.
		if value != "" {
			m.farmer.Config().AddGameToWatch(value)
			_ = m.farmer.Config().Save()
			games := m.farmer.Config().GetGamesToWatch()
			if len(games) > 0 {
				m.dropsFocusedPanel = dropsPanelGames
				m.dropsGameCursor = len(games) - 1
			}
		}
	}

	m.inputMode = inputNone
	return m, nil
}

// View implements tea.Model. Renders the persistent header + tab bar,
// then dispatches to the tab-specific view.
func (m Model) View() string {
	if m.quitting {
		return "Shutting down...\n"
	}

	// Header (visible in every tab)
	username := "..."
	if user := m.farmer.GetUser(); user != nil {
		username = user.DisplayName
	}
	stats := m.farmer.GetStats()

	header := []string{
		renderHeader(username, stats.Uptime),
		renderTabBar(m.activeTab),
		"",
	}

	// Optional update banner above the tab body.
	if banner := renderUpdateBanner(m.farmer.GetUpdateInfo()); banner != "" {
		header = append(header, banner, "")
	}

	headerStr := strings.Join(header, "\n")

	switch m.activeTab {
	case tabChannels:
		return headerStr + "\n" + m.viewChannelsTab(stats)
	case tabDrops:
		return headerStr + "\n" + m.viewDropsTab()
	case tabHelp:
		return headerStr + "\n" + m.viewHelpTab()
	}
	return headerStr
}

// viewChannelsTab renders the original channel-list / stats / drops-mini-
// table / event-log layout. The math for available-line allocation is
// the same as the pre-tab single-screen view; only the per-tab "header
// overhead" differs (header line + tab bar = 3 lines).
func (m Model) viewChannelsTab(stats farmer.Stats) string {
	var sections []string

	channels := m.farmer.GetChannels()
	drops := m.farmer.GetActiveDrops()
	dropsTable := renderDropsTable(drops, m.width)
	hasDropsTable := dropsTable != ""

	// Header overhead: header(1) + tab_bar(1) + spacer(1) = 3 lines
	// already consumed before this tab body. Tab body overhead:
	// ch_header(1) + spacer(1) + stats_border(3) + spacer(1) +
	// log_title(1) + log_border(2) + help(1) = 10, plus 2 buffer.
	overhead := 12 + 3
	if banner := renderUpdateBanner(m.farmer.GetUpdateInfo()); banner != "" {
		overhead += 2
	}
	if hasDropsTable {
		overhead += len(drops) + 3
	}

	available := m.height - overhead
	if available < 10 {
		available = 10
	}

	maxChannelRows := m.height / 2
	minLogContent := 4

	if available-maxChannelRows < minLogContent {
		maxChannelRows = available - minLogContent
	}
	if maxChannelRows < 3 {
		maxChannelRows = 3
	}

	channelRows := len(channels)
	if channelRows > maxChannelRows {
		channelRows = maxChannelRows
	}

	maxScroll := len(channels) - channelRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.channelScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}

	sections = append(sections, renderChannelTableScrollable(channels, m.width, channelRows, scroll))
	sections = append(sections, "")
	sections = append(sections, renderStatsBar(stats, m.width))
	sections = append(sections, "")

	if hasDropsTable {
		sections = append(sections, dropsTable)
		sections = append(sections, "")
	}

	logContent := available - channelRows
	if scroll > 0 {
		logContent--
	}
	if scroll < maxScroll {
		logContent--
	}
	if logContent < minLogContent {
		logContent = minLogContent
	}
	logHeight := logContent + 2

	logs := m.farmer.GetLogs()
	sections = append(sections, renderEventLog(logs, logHeight, m.width))

	if m.inputMode != inputNone {
		sections = append(sections, m.renderInput())
	} else if m.errMsg != "" && time.Now().Before(m.errExpiry) {
		sections = append(sections, lipgloss.NewStyle().Foreground(colorRed).Render("  "+m.errMsg))
	} else {
		sections = append(sections, renderHelpBar())
	}

	return strings.Join(sections, "\n")
}

// viewDropsTab renders the three stacked Drops-tab panels (Drop
// Campaigns / Wanted Games / Settings) with the unified cursor + per-
// panel help footer.
func (m Model) viewDropsTab() string {
	rows := m.farmer.GetActiveDrops()
	games := m.farmer.Config().GetGamesToWatch()
	settings := dropsSettings(m.farmer.Config())

	// Clamp cursors so a stale state from a previous render doesn't
	// point past the end of the panel's data (e.g., user removed a
	// game between renders).
	if m.dropsCampaignCursor >= len(rows) {
		m.dropsCampaignCursor = max0(len(rows) - 1)
	}
	if m.dropsGameCursor >= len(games) {
		m.dropsGameCursor = max0(len(games) - 1)
	}
	if m.dropsSettingsCursor >= len(settings) {
		m.dropsSettingsCursor = max0(len(settings) - 1)
	}

	var sections []string
	sections = append(sections,
		renderDropsCampaignsPanel(rows, m.dropsCampaignCursor, m.dropsFocusedPanel == dropsPanelCampaigns),
		"",
		renderDropsGamesPanel(games, m.dropsGameCursor, m.dropsFocusedPanel == dropsPanelGames),
		"",
		renderDropsSettingsPanel(m.farmer.Config(), settings, m.dropsSettingsCursor, m.dropsFocusedPanel == dropsPanelSettings),
		"",
	)

	if m.inputMode == inputAddGameName {
		sections = append(sections, m.renderInput())
	} else if m.errMsg != "" && time.Now().Before(m.errExpiry) {
		sections = append(sections, lipgloss.NewStyle().Foreground(colorRed).Render("  "+m.errMsg))
	} else {
		sections = append(sections, renderDropsHelpFooter(m.dropsFocusedPanel))
	}

	return strings.Join(sections, "\n")
}

// viewHelpTab renders a static reference of all keybinds + a brief
// explainer of the two credit pipelines (drops vs channel-points) so a
// first-time user understands what the tool actually does.
func (m Model) viewHelpTab() string {
	return renderHelpScreen()
}

// max0 clamps n to >= 0. Used by viewDropsTab for cursor clamping when
// the underlying data shrank between renders.
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}


func (m Model) renderInput() string {
	var prompt, hint string
	switch m.inputMode {
	case inputAddChannel:
		prompt = "Add channel: "
		hint = "  (Enter to confirm, Esc to cancel)"
	case inputRemoveChannel:
		prompt = "Remove channel: "
		hint = "  (Enter to confirm, Esc to cancel)"
	case inputSetPriority:
		prompt = "Set priority (name 1|2): "
		hint = "  (1=always watch, 2=rotate)"
	case inputAddGameName:
		prompt = "Add game name: "
		hint = "  (Enter to confirm, Esc to cancel)"
	}

	input := helpKeyStyle.Render(prompt) + m.inputValue + lipgloss.NewStyle().
		Foreground(colorPurple).
		Blink(true).
		Render("_")

	out := "  " + input + helpStyle.Render(hint)

	// v1.8.0: when adding a wanted-game name, show fuzzy matches against
	// the current cycle's eligible games so the user doesn't have to type
	// the full name correctly.
	if m.inputMode == inputAddGameName {
		query := strings.ToLower(strings.TrimSpace(m.inputValue))
		all := m.farmer.GetEligibleGames()
		var matches []string
		for _, g := range all {
			if query == "" || strings.Contains(strings.ToLower(g), query) {
				matches = append(matches, g)
				if len(matches) >= 6 {
					break
				}
			}
		}
		if len(matches) > 0 {
			out += "\n  " + helpStyle.Render("matches: ") + helpKeyStyle.Render(strings.Join(matches, " | "))
		}
	}

	return out
}

// Run starts the Bubbletea program.
func Run(f *farmer.Farmer) error {
	p := tea.NewProgram(
		NewModel(f),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return err
}
