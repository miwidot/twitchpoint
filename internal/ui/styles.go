package ui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	colorPurple   = lipgloss.Color("#9146FF") // Twitch purple
	colorGreen    = lipgloss.Color("#00E676")
	colorRed      = lipgloss.Color("#FF5252")
	colorYellow   = lipgloss.Color("#FFD740")
	colorGray     = lipgloss.Color("#888888")
	colorDarkGray = lipgloss.Color("#444444")
	colorWhite    = lipgloss.Color("#FFFFFF")
	colorCyan     = lipgloss.Color("#00BCD4")

	// Header
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			Background(colorPurple).
			Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPurple)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(colorGray)

	// Table
	tableHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorPurple).
				BorderBottom(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(colorDarkGray)

	tableCellStyle = lipgloss.NewStyle().
			Foreground(colorWhite)

	// Status indicators
	onlineStyle = lipgloss.NewStyle().
			Foreground(colorGreen).
			Bold(true)

	offlineStyle = lipgloss.NewStyle().
			Foreground(colorRed)

	watchingStyle = lipgloss.NewStyle().
			Foreground(colorCyan)

	dropStyle = lipgloss.NewStyle().
			Foreground(colorGreen).
			Bold(true)

	// Stats bar
	statsBarStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorPurple).
			Padding(0, 1)

	statLabelStyle = lipgloss.NewStyle().
			Foreground(colorGray)

	statValueStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorYellow)

	// Log
	logBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorDarkGray)

	logTimeStyle = lipgloss.NewStyle().
			Foreground(colorDarkGray)

	logMessageStyle = lipgloss.NewStyle().
			Foreground(colorWhite)

	// Help bar
	helpStyle = lipgloss.NewStyle().
			Foreground(colorGray)

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(colorPurple).
			Bold(true)

	// Update banner
	updateBannerStyle = lipgloss.NewStyle().
				Foreground(colorYellow).
				Bold(true)

	// Tab bar
	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			Background(colorPurple).
			Padding(0, 2)

	tabInactiveStyle = lipgloss.NewStyle().
				Foreground(colorGray).
				Padding(0, 2)

	// Drops-tab cursor + sections
	cursorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPurple)

	sectionTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorPurple).
				BorderBottom(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(colorDarkGray)

	sectionTitleActiveStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorWhite).
				Background(colorPurple).
				BorderBottom(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(colorPurple).
				Padding(0, 1)

	// autoTagStyle marks campaigns whose game is NOT in the user's
	// wanted_games priority list — i.e. the bot is farming them
	// because the account is linked, not because the user requested
	// the game. Cyan keeps it visually distinct from status colors
	// (green/red/grey).
	autoTagStyle = lipgloss.NewStyle().
			Foreground(colorCyan).
			Bold(true)
)
