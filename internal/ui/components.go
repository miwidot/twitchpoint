package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/miwi/twitchpoint/internal/farmer"
)

// renderHeader renders the top header bar.
func renderHeader(username string, uptime time.Duration) string {
	title := headerStyle.Render(" TwitchPoint Farmer ")
	user := subtitleStyle.Render(fmt.Sprintf(" User: %s ", username))
	uptimeStr := subtitleStyle.Render(fmt.Sprintf(" Uptime: %s ", formatDuration(uptime)))

	return lipgloss.JoinHorizontal(lipgloss.Center, title, "  ", user, "  ", uptimeStr)
}

// channelTableHeader returns the column header line for the channel table.
func channelTableHeader() string {
	header := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %*s %*s %*s %-*s",
		4, "Pri",
		16, "Channel",
		10, "Status",
		10, "Watching",
		18, "Game",
		12, "Balance",
		10, "Earned",
		7, "Claims",
		12, "Last Claim",
	)
	return tableHeaderStyle.Render(header)
}

// renderChannelRow renders a single channel row.
func renderChannelRow(ch farmer.ChannelSnapshot) string {
	const (
		priW       = 4
		nameW      = 16
		statusW    = 10
		watchW     = 10
		gameW      = 18
		balanceW   = 12
		earnedW    = 10
		claimsW    = 7
		lastClaimW = 12
	)

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
	if len(game) > gameW {
		game = game[:gameW-2] + ".."
	}
	if game == "" {
		game = "-"
	}

	name := ch.DisplayName
	if ch.IsTemporary {
		name = ch.DisplayName + " [TEMP]"
	}
	if len(name) > nameW {
		name = name[:nameW-2] + ".."
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

	row := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %*s %*s %*s %-*s",
		priW+9, pri,
		nameW, name,
		statusW+9, status,
		watchW+9, watching,
		gameW, game,
		balanceW, balance,
		earnedW, earned,
		claimsW, claims,
		lastClaimW, lastClaim,
	)
	return tableCellStyle.Render(row)
}

// renderChannelTableScrollable renders the channel table with scroll support.
func renderChannelTableScrollable(channels []farmer.ChannelSnapshot, width, maxRows, scroll int) string {
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

// renderDropsTable renders the active drop campaigns table.
func renderDropsTable(drops []farmer.ActiveDrop, width int) string {
	if len(drops) == 0 {
		return ""
	}

	// Column widths
	campaignW := 24
	gameW := 18
	progressW := 16
	channelW := 16
	statusW := 10

	// Header
	header := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s",
		campaignW, "Campaign",
		gameW, "Game",
		progressW, "Progress",
		channelW, "Channel",
		statusW, "Status",
	)
	headerLine := tableHeaderStyle.Render(header)

	// Rows
	var rows []string
	for _, drop := range drops {
		campaign := drop.CampaignName
		if len(campaign) > campaignW {
			campaign = campaign[:campaignW-2] + ".."
		}

		game := drop.GameName
		if len(game) > gameW {
			game = game[:gameW-2] + ".."
		}

		progress := "-"
		if drop.Required > 0 {
			progress = fmt.Sprintf("%d/%dmin (%d%%)", drop.Progress, drop.Required, drop.Percent)
		}
		if len(progress) > progressW {
			progress = progress[:progressW-2] + ".."
		}

		channel := drop.ChannelLogin
		if channel == "" {
			channel = "-"
		}
		if drop.IsAutoSelected && channel != "-" {
			channel += " [AUTO]"
		}
		if len(channel) > channelW {
			channel = channel[:channelW-2] + ".."
		}

		var status string
		if !drop.IsAccountConnected {
			status = lipgloss.NewStyle().Foreground(colorRed).Render("UNLINKED")
		} else if !drop.IsEnabled {
			status = subtitleStyle.Render("DISABLED")
		} else {
			status = dropStyle.Render("ACTIVE")
		}

		row := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s",
			campaignW, campaign,
			gameW, game,
			progressW, progress,
			channelW, channel,
			statusW+9, status, // +9 for ANSI escape codes
		)
		rows = append(rows, tableCellStyle.Render(row))
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

// renderHelpBar renders the bottom help bar.
func renderHelpBar() string {
	keys := []struct{ key, desc string }{
		{"q", "quit"},
		{"a", "add channel"},
		{"d", "remove channel"},
		{"p", "set priority"},
		{"↑↓", "scroll"},
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
