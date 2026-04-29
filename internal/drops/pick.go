package drops

import "strings"

// PickGameMatches returns true if the freshly-fetched game equals (case-
// insensitive) any of the pick's campaign games. Used as a guard before
// committing the watcher to a channel — streamer may have switched games
// between selector run and now.
func PickGameMatches(pick *PoolEntry, currentGame string) bool {
	for _, c := range pick.Campaigns {
		if strings.EqualFold(c.GameName, currentGame) {
			return true
		}
	}
	return false
}

// PickCampaignGames returns a comma-separated list of distinct game names
// across the pick's campaigns. Diagnostic only.
func PickCampaignGames(pick *PoolEntry) string {
	seen := make(map[string]bool, len(pick.Campaigns))
	out := make([]string, 0, len(pick.Campaigns))
	for _, c := range pick.Campaigns {
		key := strings.ToLower(c.GameName)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c.GameName)
	}
	return strings.Join(out, ",")
}
