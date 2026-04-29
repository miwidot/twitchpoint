package ui

import (
	"fmt"
	"strings"
)

// renderHelpScreen draws the static Help-tab content: tab nav, per-tab
// keybinds, and a brief drops-vs-channel-points explainer so a first-
// time user knows what the two credit pipelines are.
func renderHelpScreen() string {
	var sections []string

	sections = append(sections, titleStyle.Render(" Tab Navigation "))
	sections = append(sections, helpRow("1", "Channels tab"))
	sections = append(sections, helpRow("2", "Drops tab"))
	sections = append(sections, helpRow("3", "Help tab (this view)"))
	sections = append(sections, helpRow("Tab / Shift+Tab", "cycle tabs"))
	sections = append(sections, helpRow("q / Ctrl+C", "quit"))
	sections = append(sections, "")

	sections = append(sections, titleStyle.Render(" Channels Tab "))
	sections = append(sections, helpRow("a", "add channel"))
	sections = append(sections, helpRow("d", "remove channel"))
	sections = append(sections, helpRow("p", "set priority (name 1=always-watch | 2=rotate)"))
	sections = append(sections, helpRow("j / k or ↑ / ↓", "scroll channel table"))
	sections = append(sections, helpRow("home / end", "jump to top/bottom"))
	sections = append(sections, "")

	sections = append(sections, titleStyle.Render(" Drops Tab "))
	sections = append(sections, helpRow("j / k or ↑ / ↓", "navigate (overflows between panels)"))
	sections = append(sections, helpRow("space", "toggle (Drop Campaigns / Settings)"))
	sections = append(sections, helpRow("+", "add game (Wanted Games panel)"))
	sections = append(sections, helpRow("-", "remove game (Wanted Games panel)"))
	sections = append(sections, helpRow("u / d", "reorder game up/down (Wanted Games panel)"))
	sections = append(sections, "")

	sections = append(sections, titleStyle.Render(" How TwitchPoint farms "))
	sections = append(sections, paragraph(
		"Two independent credit pipelines run side by side:",
	))
	sections = append(sections, paragraph(
		"  Drops — the picked drop channel is owned exclusively by the drops Watcher.",
	))
	sections = append(sections, paragraph(
		"           It sends GraphQL sendSpadeEvents heartbeats every ~59 seconds and",
	))
	sections = append(sections, paragraph(
		"           polls DropCurrentSession every minute. Auto-claim fires when a drop hits 100%.",
	))
	sections = append(sections, paragraph(
		"  Channel-Points — up to 2 rotation channels are watched at a time via the legacy",
	))
	sections = append(sections, paragraph(
		"           POST spade.twitch.tv/track endpoint. Bonus claims (the chest icon) are auto-",
	))
	sections = append(sections, paragraph(
		"           claimed via PubSub. Rotation cycles through online channels every 5 minutes.",
	))
	sections = append(sections, "")
	sections = append(sections, paragraph(
		"Priority: P0 (auto, drop-active channels) → P1 (always-watch) → P2 (rotate). The drops",
	))
	sections = append(sections, paragraph(
		"Watcher's current channel is skipped by points rotation to avoid double-tracking.",
	))
	sections = append(sections, "")
	sections = append(sections, paragraph(
		"Drop campaigns marked "+autoTagStyle.Render("[AUTO]")+" are farmed automatically because",
	))
	sections = append(sections, paragraph(
		"the account is linked — they're not in your wanted_games priority list. With an empty",
	))
	sections = append(sections, paragraph(
		"wanted_games list, EVERY linked campaign is auto-discovered and the marker is hidden.",
	))

	return strings.Join(sections, "\n")
}

// helpRow renders one keybind line: purple key + grey description.
func helpRow(key, desc string) string {
	return "  " + helpKeyStyle.Render(fmt.Sprintf("%-18s", key)) + helpStyle.Render(desc)
}

// paragraph wraps a description line with the help-text colors.
func paragraph(text string) string {
	return helpStyle.Render("  " + text)
}
