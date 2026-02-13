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

// renderChannelTable renders the channel status table.
func renderChannelTable(channels []farmer.ChannelSnapshot, width int) string {
	if len(channels) == 0 {
		return subtitleStyle.Render("  No channels configured. Press 'a' to add a channel.")
	}

	// Column widths
	priW := 4
	nameW := 16
	statusW := 10
	watchW := 10
	gameW := 18
	balanceW := 12
	earnedW := 10
	claimsW := 7
	lastClaimW := 12

	// Header
	header := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %*s %*s %*s %-*s",
		priW, "Pri",
		nameW, "Channel",
		statusW, "Status",
		watchW, "Watching",
		gameW, "Game",
		balanceW, "Balance",
		earnedW, "Earned",
		claimsW, "Claims",
		lastClaimW, "Last Claim",
	)
	headerLine := tableHeaderStyle.Render(header)

	// Rows
	var rows []string
	for _, ch := range channels {
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
			priW+9, pri, // +9 for ANSI escape codes
			nameW, name,
			statusW+9, status,
			watchW+9, watching,
			gameW, game,
			balanceW, balance,
			earnedW, earned,
			claimsW, claims,
			lastClaimW, lastClaim,
		)
		rows = append(rows, tableCellStyle.Render(row))
	}

	return headerLine + "\n" + strings.Join(rows, "\n")
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
