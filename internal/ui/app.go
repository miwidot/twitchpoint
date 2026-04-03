package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/miwi/twitchpoint/internal/farmer"
)

// tickMsg is sent periodically to refresh the UI.
type tickMsg time.Time

// Model is the Bubbletea app model.
type Model struct {
	farmer *farmer.Farmer
	width  int
	height int

	// Channel table scroll
	channelScroll int

	// Input mode
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

type inputState int

const (
	inputNone inputState = iota
	inputAddChannel
	inputRemoveChannel
	inputSetPriority
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
	// If in input mode, handle text input
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

	// Normal mode
	switch msg.String() {
	case "q", "ctrl+c":
		if m.OnQuit != nil {
			m.OnQuit()
			return m, nil
		}
		m.quitting = true
		m.farmer.Stop()
		return m, tea.Quit
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
	}

	m.inputMode = inputNone
	return m, nil
}

// View implements tea.Model.
func (m Model) View() string {
	if m.quitting {
		return "Shutting down...\n"
	}

	var sections []string

	// Header
	username := "..."
	uptime := time.Duration(0)
	if user := m.farmer.GetUser(); user != nil {
		username = user.DisplayName
	}
	stats := m.farmer.GetStats()
	uptime = stats.Uptime

	sections = append(sections, renderHeader(username, uptime))
	sections = append(sections, "") // spacer

	// Update banner (if available)
	updateInfo := m.farmer.GetUpdateInfo()
	updateBanner := renderUpdateBanner(updateInfo)
	hasUpdateBanner := updateBanner != ""
	if hasUpdateBanner {
		sections = append(sections, updateBanner)
		sections = append(sections, "") // spacer
	}

	// Gather data
	channels := m.farmer.GetChannels()
	drops := m.farmer.GetActiveDrops()
	dropsTable := renderDropsTable(drops, m.width)
	hasDropsTable := dropsTable != ""

	// Fixed overhead: everything except channel rows and event log content
	// header(1) + spacer(1) + ch_header(1) + spacer(1) + stats_border(3) + spacer(1)
	// + log_title(1) + log_border(2) + help(1) = 12, plus 2 buffer for join newlines
	overhead := 14
	if hasUpdateBanner {
		overhead += 2 // banner + spacer
	}
	if hasDropsTable {
		overhead += len(drops) + 3 // title + header + rows + spacer
	}

	// Available lines for channel rows + event log content
	available := m.height - overhead
	if available < 10 {
		available = 10
	}

	// Channel table gets at most half the screen, event log gets the rest
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

	// Clamp scroll offset
	maxScroll := len(channels) - channelRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.channelScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}

	// Channel table (with scroll)
	sections = append(sections, renderChannelTableScrollable(channels, m.width, channelRows, scroll))
	sections = append(sections, "") // spacer

	// Stats bar
	sections = append(sections, renderStatsBar(stats, m.width))
	sections = append(sections, "") // spacer

	// Drops table (if any active campaigns)
	if hasDropsTable {
		sections = append(sections, dropsTable)
		sections = append(sections, "") // spacer
	}

	// Event log gets remaining space
	logContent := available - channelRows
	// Account for scroll indicator lines
	if scroll > 0 {
		logContent--
	}
	if scroll < maxScroll {
		logContent--
	}
	if logContent < minLogContent {
		logContent = minLogContent
	}
	// logHeight = content lines + 2 (for border accounting in renderEventLog)
	logHeight := logContent + 2

	logs := m.farmer.GetLogs()
	sections = append(sections, renderEventLog(logs, logHeight, m.width))

	// Input or error or help bar
	if m.inputMode != inputNone {
		sections = append(sections, m.renderInput())
	} else if m.errMsg != "" && time.Now().Before(m.errExpiry) {
		sections = append(sections, lipgloss.NewStyle().Foreground(colorRed).Render("  "+m.errMsg))
	} else {
		sections = append(sections, renderHelpBar())
	}

	return strings.Join(sections, "\n")
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
	}

	input := helpKeyStyle.Render(prompt) + m.inputValue + lipgloss.NewStyle().
		Foreground(colorPurple).
		Blink(true).
		Render("_")

	return "  " + input + helpStyle.Render(hint)
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
