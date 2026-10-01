package twitch

import (
	"fmt"
	"time"
)

// Persisted query hash for DropsHighlightService_AvailableDrops — the drop
// campaigns Twitch advertises on a specific channel's page. Unlike
// ViewerDropsDashboard it keeps working for Smart-TV tokens, which is what
// makes TV-client discovery possible at all.
const availableDropsHash = "782dad0f032942260171d2d80a654f88bdd0c5a9dddc392e9bc92218a0f42d20"

// tvMaxChannelsPerGame caps how many of a game's live channels we ask for
// drop campaigns. The directory is sorted by viewer count, and a campaign
// advertised on a game's biggest streams is the same campaign everywhere,
// so the top handful finds nearly everything while keeping the request
// count bounded (games × this, per inventory cycle).
const tvMaxChannelsPerGame = 10

// discoverCampaignsByGame finds drop campaigns WITHOUT ViewerDropsDashboard,
// which returns `dropCampaigns: null` for Smart-TV tokens.
//
// For every wanted game it pulls the game's live drops-enabled channels and
// asks each one which campaigns it advertises. That inverts the normal
// lookup — instead of "which campaigns exist, and where can I watch them?"
// it asks "what is live right now, and what does it award?" — and happens to
// hand back a usable (campaign → live channel) pair for free, which is
// exactly what the selector needs anyway.
//
// Known limits, inherent to the approach:
//   - only games in wantedGames are searched; with no wanted games there is
//     nothing to iterate and discovery returns empty
//   - a campaign running only on channels outside a game's top
//     tvMaxChannelsPerGame is missed until it shows up in the inventory
//   - the response carries no per-user state (no isClaimed, no progress, no
//     account-link) and no campaign start time; GetDropsInventory merges
//     that in from the inventory afterwards
func (g *GQLClient) discoverCampaignsByGame(wantedGames []string) ([]DropCampaign, error) {
	if len(wantedGames) == 0 {
		return nil, fmt.Errorf("tv discovery: no wanted games configured — nothing to search")
	}

	byID := make(map[string]*DropCampaign)
	var firstErr error
	for _, game := range wantedGames {
		slug := SlugFromGameName(game)
		streams, err := g.GetGameStreamsDropsEnabled(slug, tvMaxChannelsPerGame)
		if err != nil {
			g.diag("[Drops/TV] directory for %q failed: %v", game, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, s := range streams {
			campaigns, err := g.availableDropsForChannel(s.BroadcasterID)
			if err != nil {
				g.diag("[Drops/TV] available drops for %s failed: %v", s.BroadcasterLogin, err)
				continue
			}
			for i := range campaigns {
				c := campaigns[i]
				// The response's own game field is frequently null, so take
				// the game from the directory query we just made — we know
				// which game's channels these are.
				c.GameName = game
				c.GameSlug = slug
				if s.GameID != "" {
					c.GameID = s.GameID
				}
				existing, seen := byID[c.ID]
				if !seen {
					byID[c.ID] = &c
					existing = &c
				}
				// Record the live channel we found it on. Discovery via a
				// specific channel means we always have at least one
				// watchable channel per campaign, without an allow-list.
				existing.Channels = appendChannelOnce(existing.Channels, DropChannel{
					ID:          s.BroadcasterID,
					Name:        s.BroadcasterLogin,
					DisplayName: s.DisplayName,
				})
			}
		}
	}

	if len(byID) == 0 && firstErr != nil {
		return nil, fmt.Errorf("tv discovery: no campaigns and directory lookups failed: %w", firstErr)
	}

	out := make([]DropCampaign, 0, len(byID))
	for _, c := range byID {
		out = append(out, *c)
	}
	g.diag("[Drops/TV] discovered %d campaign(s) across %d game(s)", len(out), len(wantedGames))
	return out, nil
}

func appendChannelOnce(list []DropChannel, ch DropChannel) []DropChannel {
	for _, existing := range list {
		if existing.ID == ch.ID {
			return list
		}
	}
	return append(list, ch)
}

// availableDropsForChannel asks a single channel which drop campaigns it
// advertises. Sub-reward campaigns are filtered out — they award on
// subscriptions, not watch time, so there is nothing for the miner to farm.
func (g *GQLClient) availableDropsForChannel(channelID string) ([]DropCampaign, error) {
	req := &GQLRequest{
		OperationName: "DropsHighlightService_AvailableDrops",
		Variables:     map[string]interface{}{"channelID": channelID},
		Extensions: &GQLExtensions{
			PersistedQuery: &PersistedQuery{Version: 1, SHA256Hash: availableDropsHash},
		},
	}
	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("available drops: %w", err)
	}

	channel, ok := resp.Data["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return nil, nil
	}
	raw, ok := channel["viewerDropCampaigns"].([]interface{})
	if !ok {
		// null means the channel advertises nothing — not an error.
		return nil, nil
	}

	var out []DropCampaign
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		c := parseAvailableDropCampaign(m)
		if c == nil {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

// parseAvailableDropTime parses the RFC3339 timestamps this query returns.
// A missing or unparsable value yields the zero time, which downstream
// window checks already treat as "no bound".
func parseAvailableDropTime(m map[string]interface{}, key string) time.Time {
	t, err := time.Parse(time.RFC3339, getString(m, key))
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseAvailableDropCampaign converts one viewerDropCampaigns entry. Returns
// nil for campaigns the miner cannot farm.
func parseAvailableDropCampaign(m map[string]interface{}) *DropCampaign {
	id := getString(m, "id")
	if id == "" {
		return nil
	}

	// Skip campaigns that award on subscriptions rather than watch time.
	// They come back with requiredSubs set and requiredMinutesWatched 0, and
	// no amount of watching will ever earn them.
	if summary, ok := m["summary"].(map[string]interface{}); ok {
		if mw, ok := summary["includesMWRequirement"].(bool); ok && !mw {
			return nil
		}
	}

	c := &DropCampaign{
		ID:    id,
		Name:  getString(m, "name"),
		EndAt: parseAvailableDropTime(m, "endAt"),
		// This query exposes no per-user state. Both fields are corrected
		// from the inventory during the merge; assuming "connected" here
		// keeps a campaign visible until the inventory says otherwise,
		// matching how details-sourced campaigns are treated.
		IsAccountConnected: true,
		Status:             "ACTIVE",
	}

	drops, _ := m["timeBasedDrops"].([]interface{})
	for _, d := range drops {
		dm, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		required := getInt(dm, "requiredMinutesWatched")
		if required <= 0 {
			continue // sub-only or malformed drop — nothing to watch for
		}
		drop := TimeBasedDrop{
			ID:                     getString(dm, "id"),
			Name:                   getString(dm, "name"),
			RequiredMinutesWatched: required,
			StartAt:                parseAvailableDropTime(dm, "startAt"),
			EndAt:                  parseAvailableDropTime(dm, "endAt"),
		}
		if edges, ok := dm["benefitEdges"].([]interface{}); ok && len(edges) > 0 {
			if edge, ok := edges[0].(map[string]interface{}); ok {
				if benefit, ok := edge["benefit"].(map[string]interface{}); ok {
					drop.BenefitID = getString(benefit, "id")
					drop.BenefitName = getString(benefit, "name")
					drop.BenefitType = getString(benefit, "distributionType")
				}
			}
		}
		c.Drops = append(c.Drops, drop)
	}

	if len(c.Drops) == 0 {
		return nil // nothing watch-based to farm here
	}
	return c
}
