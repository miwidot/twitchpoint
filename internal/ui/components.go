package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/miwi/twitchpoint/internal/channels"
	"github.com/miwi/twitchpoint/internal/drops"
	"github.com/miwi/twitchpoint/internal/farmer"
)

// renderTabBar renders the top-level tab navigation strip. The active tab
// gets the purple background; inactive tabs are grey. Number prefix is
// the direct-jump key (1/2/3); Tab/Shift-Tab also cycles.
func renderTabBar(active tabID) string {
	tabs := []struct {
		id    tabID
		label string
	}{
		{tabChannels, "1 Channels"},
		{tabDrops, "2 Drops"},
		{tabHelp, "3 Help"},
	}
	var rendered []string
	for _, t := range tabs {
		if t.id == active {
			rendered = append(rendered, tabActiveStyle.Render(t.label))
		} else {
			rendered = append(rendered, tabInactiveStyle.Render(t.label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}

// renderHeader renders the top header bar.
func renderHeader(username string, uptime time.Duration) string {
	title := headerStyle.Render(" TwitchPoint Farmer ")
	user := subtitleStyle.Render(fmt.Sprintf(" User: %s ", username))
	uptimeStr := subtitleStyle.Render(fmt.Sprintf(" Uptime: %s ", formatDuration(uptime)))

	return lipgloss.JoinHorizontal(lipgloss.Center, title, "  ", user, "  ", uptimeStr)
}

// Channel-table column widths. Used by both the header and row renderers.
const (
	chColPri       = 4
	chColName      = 16
	chColStatus    = 10
	chColWatch     = 10
	chColGame      = 18
	chColBalance   = 12
	chColEarned    = 10
	chColClaims    = 7
	chColLastClaim = 12
)

// padCell wraps content in a fixed-width box. Uses lipgloss for the width
// computation, which counts visible chars (strips ANSI escapes) — this
// is what makes columns align properly when some cells are styled and
// some aren't. The pre-Phase-4 code used `fmt.Sprintf("%-*s", w+9, …)`
// with +9 as a pauschal ANSI-overhead estimate; that's wrong because
// lipgloss styles emit different escape-byte counts depending on which
// attributes are set (Bold + Foreground = more bytes than just
// Foreground), so the +9 was right for some cells and wrong for others.
func padCell(content string, width int, alignRight bool) string {
	align := lipgloss.Left
	if alignRight {
		align = lipgloss.Right
	}
	return lipgloss.NewStyle().Width(width).Align(align).Render(content)
}

// channelTableHeader returns the column header line for the channel table.
func channelTableHeader() string {
	cells := []string{
		padCell(tableHeaderStyle.Render("Pri"),       chColPri,       false),
		padCell(tableHeaderStyle.Render("Channel"),   chColName,      false),
		padCell(tableHeaderStyle.Render("Status"),    chColStatus,    false),
		padCell(tableHeaderStyle.Render("Watching"),  chColWatch,     false),
		padCell(tableHeaderStyle.Render("Game"),      chColGame,      false),
		padCell(tableHeaderStyle.Render("Balance"),   chColBalance,   true),
		padCell(tableHeaderStyle.Render("Earned"),    chColEarned,    true),
		padCell(tableHeaderStyle.Render("Claims"),    chColClaims,    true),
		padCell(tableHeaderStyle.Render("Last Claim"), chColLastClaim, false),
	}
	return "  " + strings.Join(cells, " ")
}

// renderChannelRow renders a single channel row.
func renderChannelRow(ch channels.Snapshot) string {
	pri := subtitleStyle.Render("P2")
	if ch.HasActiveDrop {
		pri = dropStyle.Render("P0")
	} else if ch.Priority == 1 {
		pri = statValueStyle.Render("P1")
	}

	status := offlineStyle.Render("OFFLINE")
	if ch.IsOnline {
		status = onlineStyle.Render("LIVE")
	}

	watching := subtitleStyle.Render("-")
	if ch.IsWatching {
		watching = watchingStyle.Render("ACTIVE")
	}

	game := ch.GameName
	if ch.HasActiveDrop && ch.DropRequired > 0 {
		pct := (ch.DropProgress * 100) / ch.DropRequired
		game = fmt.Sprintf("%s %d%%", ch.GameName, pct)
	}
	if len(game) > chColGame {
		game = game[:chColGame-2] + ".."
	}
	if game == "" {
		game = "-"
	}

	name := ch.DisplayName
	if ch.IsTemporary {
		name = ch.DisplayName + " [TEMP]"
	}
	if len(name) > chColName {
		name = name[:chColName-2] + ".."
	}

	balance := "-"
	if ch.PointsBalance > 0 {
		balance = formatNumber(ch.PointsBalance)
	}

	earned := "-"
	if ch.PointsEarnedSession > 0 {
		earned = fmt.Sprintf("+%s", formatNumber(ch.PointsEarnedSession))
	}

	claims := "-"
	if ch.ClaimsMade > 0 {
		claims = fmt.Sprintf("%d", ch.ClaimsMade)
	}

	lastClaim := "-"
	if !ch.LastClaimTime.IsZero() {
		lastClaim = formatTimeAgo(ch.LastClaimTime)
	}

	cells := []string{
		padCell(pri,       chColPri,       false),
		padCell(name,      chColName,      false),
		padCell(status,    chColStatus,    false),
		padCell(watching,  chColWatch,     false),
		padCell(game,      chColGame,      false),
		padCell(balance,   chColBalance,   true),
		padCell(earned,    chColEarned,    true),
		padCell(claims,    chColClaims,    true),
		padCell(lastClaim, chColLastClaim, false),
	}
	return "  " + strings.Join(cells, " ")
}

// renderChannelTableScrollable renders the channel table with scroll support.
func renderChannelTableScrollable(channels []channels.Snapshot, width, maxRows, scroll int) string {
	if len(channels) == 0 {
		return subtitleStyle.Render("  No channels configured. Press 'a' to add a channel.")
	}

	var parts []string
	parts = append(parts, channelTableHeader())

	// Scroll indicator top
	if scroll > 0 {
		parts = append(parts, subtitleStyle.Render(fmt.Sprintf("  ▲ %d more", scroll)))
	}

	// Visible rows
	end := scroll + maxRows
	if end > len(channels) {
		end = len(channels)
	}
	for _, ch := range channels[scroll:end] {
		parts = append(parts, renderChannelRow(ch))
	}

	// Scroll indicator bottom
	remaining := len(channels) - end
	if remaining > 0 {
		parts = append(parts, subtitleStyle.Render(fmt.Sprintf("  ▼ %d more", remaining)))
	}

	return strings.Join(parts, "\n")
}

// Drops mini-table column widths (Channels-tab ACTIVE-only mini-view).
const (
	dColCampaign = 24
	dColGame     = 18
	dColProgress = 16
	dColChannel  = 16
	dColStatus   = 10
)

// renderDropsTable renders the active drop campaigns table.
func renderDropsTable(drops []drops.ActiveDrop, width int) string {
	if len(drops) == 0 {
		return ""
	}

	headerCells := []string{
		padCell(tableHeaderStyle.Render("Campaign"), dColCampaign, false),
		padCell(tableHeaderStyle.Render("Game"),     dColGame,     false),
		padCell(tableHeaderStyle.Render("Progress"), dColProgress, false),
		padCell(tableHeaderStyle.Render("Channel"),  dColChannel,  false),
		padCell(tableHeaderStyle.Render("Status"),   dColStatus,   false),
	}
	headerLine := "  " + strings.Join(headerCells, " ")

	var rows []string
	for _, drop := range drops {
		campaign := drop.CampaignName
		if len(campaign) > dColCampaign {
			campaign = campaign[:dColCampaign-2] + ".."
		}

		game := drop.GameName
		if len(game) > dColGame {
			game = game[:dColGame-2] + ".."
		}

		progress := "-"
		if drop.Required > 0 {
			progress = fmt.Sprintf("%d/%dmin (%d%%)", drop.Progress, drop.Required, drop.Percent)
		}
		if len(progress) > dColProgress {
			progress = progress[:dColProgress-2] + ".."
		}

		channel := drop.ChannelLogin
		if channel == "" {
			channel = "-"
		}
		if drop.IsAutoSelected && channel != "-" {
			channel += " [AUTO]"
		}
		if len(channel) > dColChannel {
			channel = channel[:dColChannel-2] + ".."
		}

		statusLabel := drop.Status
		if statusLabel == "" {
			if !drop.IsEnabled {
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

		cells := []string{
			padCell(campaign, dColCampaign, false),
			padCell(game,     dColGame,     false),
			padCell(progress, dColProgress, false),
			padCell(channel,  dColChannel,  false),
			padCell(status,   dColStatus,   false),
		}
		rendered := "  " + strings.Join(cells, " ")
		if drop.IsAutoDiscovered {
			rendered += "  " + autoTagStyle.Render("[AUTO]")
		}
		rows = append(rows, rendered)
	}

	title := titleStyle.Render(" Drop Campaigns ")
	return title + "\n" + headerLine + "\n" + strings.Join(rows, "\n")
}

// renderStatsBar renders the aggregate stats bar.
func renderStatsBar(stats farmer.Stats, width int) string {
	items := []string{
		statLabelStyle.Render("Points Earned: ") + statValueStyle.Render(formatNumber(stats.TotalPointsEarned)),
		statLabelStyle.Render("Claims: ") + statValueStyle.Render(fmt.Sprintf("%d", stats.TotalClaimsMade)),
		statLabelStyle.Render("Online: ") + statValueStyle.Render(fmt.Sprintf("%d/%d", stats.ChannelsOnline, stats.ChannelsTotal)),
		statLabelStyle.Render("Watching: ") + statValueStyle.Render(fmt.Sprintf("%d/2", stats.ChannelsWatching)),
		statLabelStyle.Render("Drops: ") + dropStyle.Render(fmt.Sprintf("%d", stats.ActiveDrops)),
	}

	content := strings.Join(items, "    ")
	return statsBarStyle.Width(width - 2).Render(content)
}

// renderEventLog renders the scrollable event log.
func renderEventLog(logs []farmer.LogEntry, height, width int) string {
	if height < 3 {
		height = 3
	}

	visibleLines := height - 2 // Account for border

	var lines []string

	// Show the most recent logs that fit
	start := 0
	if len(logs) > visibleLines {
		start = len(logs) - visibleLines
	}

	for i := start; i < len(logs); i++ {
		entry := logs[i]
		timeStr := logTimeStyle.Render(entry.Time.Format("15:04:05"))
		msg := logMessageStyle.Render(entry.Message)
		line := fmt.Sprintf(" %s  %s", timeStr, msg)

		// Truncate if too wide
		if len(line) > width-4 {
			line = line[:width-7] + "..."
		}
		lines = append(lines, line)
	}

	// Pad remaining lines
	for len(lines) < visibleLines {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")
	title := titleStyle.Render(" Event Log ")

	return title + "\n" + logBorderStyle.Width(width - 2).Height(visibleLines).Render(content)
}

// renderHelpBar renders the Channels-tab footer help line. Drops-tab
// has its own per-panel footer (renderDropsHelpFooter) and Help-tab
// is itself the keybind reference, so this is Channels-only.
func renderHelpBar() string {
	keys := []struct{ key, desc string }{
		{"a", "add channel"},
		{"d", "remove channel"},
		{"p", "set priority"},
		{"↑↓", "scroll"},
		{"1/2/3", "tab"},
		{"q", "quit"},
	}

	var parts []string
	for _, k := range keys {
		parts = append(parts, helpKeyStyle.Render(k.key)+helpStyle.Render(" "+k.desc))
	}

	return helpStyle.Render("  " + strings.Join(parts, "  |  "))
}

// Helper functions

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

// renderUpdateBanner renders a yellow update notification line.
func renderUpdateBanner(info farmer.UpdateInfo) string {
	if !info.HasStableUpdate && !info.HasBetaUpdate {
		return ""
	}

	var parts []string
	if info.HasStableUpdate {
		parts = append(parts, fmt.Sprintf("v%s (stable)", info.LatestStable))
	}
	if info.HasBetaUpdate {
		parts = append(parts, fmt.Sprintf("v%s (beta)", info.LatestBeta))
	}

	text := fmt.Sprintf("  New version available: %s", strings.Join(parts, " and "))
	return updateBannerStyle.Render(text)
}

