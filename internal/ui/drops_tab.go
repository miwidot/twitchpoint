package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/drops"
)

// dropsSetting binds a UI label to a getter+toggle pair on Config. Used
// by the Drops-tab Settings panel to render rows + handle Space-toggle
// without the panel code having to know which Config field each row
// represents.
type dropsSetting struct {
	label   string
	get     func(*config.Config) bool
	toggle  func(*config.Config)
	restart bool // true if the toggle takes effect on next farmer restart only
}

// dropsSettings returns the runtime-toggleable boot config flags. All
// three currently require a farmer restart to take effect — the Drops
// tab's Space-toggle persists the change to config.json immediately
// and the (restart required) hint reminds the user.
func dropsSettings(_ *config.Config) []dropsSetting {
	return []dropsSetting{
		{
			label:   "Drops mining enabled",
			get:     func(c *config.Config) bool { return c.DropsEnabled },
			toggle:  func(c *config.Config) { c.DropsEnabled = !c.DropsEnabled },
			restart: true,
		},
		{
			label:   "IRC viewer presence",
			get:     func(c *config.Config) bool { return c.IrcEnabled },
			toggle:  func(c *config.Config) { c.IrcEnabled = !c.IrcEnabled },
			restart: true,
		},
		{
			label:   "Web UI enabled",
			get:     func(c *config.Config) bool { return c.WebEnabled },
			toggle:  func(c *config.Config) { c.WebEnabled = !c.WebEnabled },
			restart: true,
		},
	}
}

// renderDropsCampaignsPanel draws the Drop Campaigns panel of the Drops
// tab — a cursor-driven version of the existing renderDropsTable. The
// active row gets a ▸ marker + cursor highlight when this panel is
// focused.
func renderDropsCampaignsPanel(rows []drops.ActiveDrop, cursor int, focused bool) string {
	title := renderPanelTitle("Drop Campaigns", focused)

	if len(rows) == 0 {
		body := tableCellStyle.Render("    (no campaigns in current inventory cycle)")
		return title + "\n" + body
	}

	const (
		campaignW = 24
		gameW     = 18
		progressW = 16
		channelW  = 16
		statusW   = 10
	)
	header := fmt.Sprintf("    %-*s %-*s %-*s %-*s %-*s",
		campaignW, "Campaign",
		gameW, "Game",
		progressW, "Progress",
		channelW, "Channel",
		statusW, "Status",
	)
	headerLine := tableHeaderStyle.Render(header)

	var renderedRows []string
	for i, d := range rows {
		marker := "  "
		if focused && i == cursor {
			marker = cursorStyle.Render("▸ ")
		}
		campaign := truncate(d.CampaignName, campaignW)
		game := truncate(d.GameName, gameW)
		progress := "-"
		if d.Required > 0 {
			progress = fmt.Sprintf("%d/%dmin (%d%%)", d.Progress, d.Required, d.Percent)
		}
		progress = truncate(progress, progressW)
		channel := d.ChannelLogin
		if channel == "" {
			channel = "-"
		}
		if d.IsAutoSelected && channel != "-" {
			channel += " [AUTO]"
		}
		channel = truncate(channel, channelW)

		statusLabel := d.Status
		if statusLabel == "" {
			if !d.IsEnabled {
				statusLabel = "DISABLED"
			} else {
				statusLabel = "ACTIVE"
			}
		}
		var status string
		switch statusLabel {
		case "ACTIVE":
			status = dropStyle.Render(statusLabel)
		default:
			status = subtitleStyle.Render(statusLabel)
		}

		row := fmt.Sprintf("%s %-*s %-*s %-*s %-*s %-*s",
			marker,
			campaignW, campaign,
			gameW, game,
			progressW, progress,
			channelW, channel,
			statusW+9, status,
		)
		if focused && i == cursor {
			renderedRows = append(renderedRows, lipgloss.NewStyle().Foreground(colorWhite).Bold(true).Render(row))
		} else {
			renderedRows = append(renderedRows, tableCellStyle.Render(row))
		}
	}

	return title + "\n" + headerLine + "\n" + strings.Join(renderedRows, "\n")
}

// renderDropsGamesPanel draws the Wanted Games panel — priority-ordered
// list with cursor + reorder hints in the panel footer when focused.
func renderDropsGamesPanel(games []string, cursor int, focused bool) string {
	title := renderPanelTitle("Wanted Games (priority order)", focused)

	if len(games) == 0 {
		body := tableCellStyle.Render("    (no games yet — press ") +
			helpKeyStyle.Render("+") +
			tableCellStyle.Render(" to add one)")
		return title + "\n" + body
	}

	var rows []string
	for i, g := range games {
		marker := "  "
		row := fmt.Sprintf("%s%2d. %s", marker, i+1, g)
		if focused && i == cursor {
			rows = append(rows, cursorStyle.Render("▸ ")+lipgloss.NewStyle().Foreground(colorWhite).Bold(true).Render(fmt.Sprintf("%2d. %s", i+1, g)))
		} else {
			rows = append(rows, tableCellStyle.Render(row))
		}
	}

	return title + "\n" + strings.Join(rows, "\n")
}

// renderDropsSettingsPanel draws boolean config toggles. Each row shows
// the current state as [x] / [ ] and (restart required) when applicable.
func renderDropsSettingsPanel(cfg *config.Config, settings []dropsSetting, cursor int, focused bool) string {
	title := renderPanelTitle("Settings", focused)

	var rows []string
	for i, s := range settings {
		box := "[ ]"
		if s.get(cfg) {
			box = "[x]"
		}
		hint := ""
		if s.restart {
			hint = subtitleStyle.Render("  (restart required)")
		}
		marker := "  "
		body := fmt.Sprintf("%s%s %s", marker, box, s.label)
		if focused && i == cursor {
			rows = append(rows, cursorStyle.Render("▸ ")+lipgloss.NewStyle().Foreground(colorWhite).Bold(true).Render(fmt.Sprintf("%s %s", box, s.label))+hint)
		} else {
			rows = append(rows, tableCellStyle.Render(body)+hint)
		}
	}

	return title + "\n" + strings.Join(rows, "\n")
}

// renderPanelTitle is the per-panel header — purple background when the
// panel is focused, otherwise plain title style. The visual shift is
// what tells the user which panel j/k currently navigates.
func renderPanelTitle(name string, focused bool) string {
	if focused {
		return sectionTitleActiveStyle.Render(" " + name + " ")
	}
	return sectionTitleStyle.Render(" " + name + " ")
}

// renderDropsHelpFooter shows the keybind hints. The Wanted Games
// actions (+, -, u/d) auto-focus the Games panel on press, so they
// work from anywhere in the Drops tab — listed in every footer.
// Per-panel space-toggle behavior differs (campaigns vs settings),
// so that hint changes based on focused panel.
func renderDropsHelpFooter(focused dropsPanel) string {
	var parts []string

	switch focused {
	case dropsPanelCampaigns:
		parts = append(parts, helpKeyStyle.Render("space")+helpStyle.Render(" toggle disable"))
	case dropsPanelSettings:
		parts = append(parts, helpKeyStyle.Render("space")+helpStyle.Render(" toggle setting"))
	case dropsPanelGames:
		// Games panel — Space is a no-op, so don't list it; +/-/u/d
		// are listed in the always-visible block below.
	}

	always := []struct{ k, d string }{
		{"+", "add game"},
		{"-", "remove game"},
		{"u/d", "reorder"},
		{"j/k", "navigate"},
		{"1/2/3", "tab"},
		{"q", "quit"},
	}
	for _, a := range always {
		parts = append(parts, helpKeyStyle.Render(a.k)+helpStyle.Render(" "+a.d))
	}

	return helpStyle.Render("  " + strings.Join(parts, "  |  "))
}

// truncate cuts s to fit width, appending ".." when truncation happens.
// Used by the campaigns table cells to keep column alignment stable
// when game/campaign names are long.
func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 2 {
		return s[:width]
	}
	return s[:width-2] + ".."
}
