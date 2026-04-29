package farmer

import (
	"sort"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// streamSource is the minimal GQL interface the selector needs. Mocked in tests.
type streamSource interface {
	GetGameStreamsDropsEnabled(gameName string, limit int) ([]twitch.GameStream, error)
	// GetChannelInfos resolves stream info for a batch of logins in parallel.
	// Used for ACL campaigns: query the campaign's allowed_channels directly
	// instead of relying on the (often too small) game-directory top 100.
	GetChannelInfos(logins []string) []*twitch.ChannelInfo
}

// CampaignRef is a lightweight reference to a campaign that a pool entry serves.
type CampaignRef struct {
	ID            string
	Name          string
	GameName      string
	EndAt         time.Time
	RemainingTime time.Duration
	IsPinned      bool
}

// PoolEntry represents one candidate channel in the selector's pool.
// A channel may serve multiple campaigns simultaneously (Twitch credits
// whichever can_earn while we watch).
//
// BroadcastID is intentionally NOT carried here — addTemporaryChannel fetches
// the live broadcast ID via GetChannelInfo when it registers the channel.
type PoolEntry struct {
	ChannelID    string        // Twitch broadcaster user ID
	ChannelLogin string        // lowercase login
	DisplayName  string
	ViewerCount  int
	Campaigns    []CampaignRef // 1+ eligible campaigns this channel serves; sorted with highest priority first
}

// DropSelector is a pure pick-a-channel function with no side effects on Farmer state.
// Construction takes everything it needs as dependencies so tests can substitute mocks.
type DropSelector struct {
	cfg     *config.Config
	streams streamSource
	now     func() time.Time // injectable for deterministic tests
}

// NewDropSelector constructs a selector with the production stream source.
func NewDropSelector(cfg *config.Config, gql *twitch.GQLClient) *DropSelector {
	return &DropSelector{
		cfg:     cfg,
		streams: gql,
		now:     time.Now,
	}
}

// filterEligibleCampaigns drops campaigns that are not currently farmable:
// non-active status, expired, account not connected, disabled by user,
// already completed, or have no watchable (non-sub-only, non-claimed) drops.
func (s *DropSelector) filterEligibleCampaigns(campaigns []twitch.DropCampaign) []twitch.DropCampaign {
	now := s.now()
	out := make([]twitch.DropCampaign, 0, len(campaigns))

	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.EndAt.IsZero() && !c.EndAt.After(now) {
			continue
		}
		if !c.IsAccountConnected {
			continue
		}
		if s.cfg.IsCampaignDisabled(c.ID) {
			continue
		}
		if s.cfg.IsCampaignCompleted(c.ID) {
			continue
		}
		// Need at least one watchable, unclaimed drop
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched > 0 && !d.IsClaimed {
				hasWatchable = true
				break
			}
		}
		if !hasWatchable {
			continue
		}
		out = append(out, c)
	}

	return out
}

// buildPool turns an eligible-campaign list into a deduped pool of candidate
// channels. For campaigns with an allow list, it queries the drops-enabled
// game directory and intersects with that list. For unrestricted campaigns,
// the top drops-enabled streams for the game become candidates directly.
//
// Channels appearing in multiple campaigns are deduped — a single PoolEntry
// carries all the campaigns it serves.
func (s *DropSelector) buildPool(eligible []twitch.DropCampaign) []*PoolEntry {
	pinnedID := s.cfg.GetPinnedCampaign()

	// Game-name → cached directory result, so we hit GQL at most once per game per cycle.
	dirCache := make(map[string][]twitch.GameStream)
	getDir := func(gameName string) []twitch.GameStream {
		if cached, ok := dirCache[gameName]; ok {
			return cached
		}
		streams, err := s.streams.GetGameStreamsDropsEnabled(gameName, 100)
		if err != nil {
			dirCache[gameName] = nil // negative cache for the cycle
			return nil
		}
		dirCache[gameName] = streams
		return streams
	}

	byChannel := make(map[string]*PoolEntry) // channelID → entry

	for _, c := range eligible {
		ref := CampaignRef{
			ID:            c.ID,
			Name:          c.Name,
			GameName:      c.GameName,
			EndAt:         c.EndAt,
			RemainingTime: time.Until(c.EndAt),
			IsPinned:      c.ID == pinnedID,
		}
		if c.GameName == "" {
			continue // can't pick without a game
		}

		hasAllow := len(c.Channels) > 0

		if hasAllow {
			// ACL/Partner-Only campaigns (e.g., ABI Partner-Only Drops):
			// query the allowed_channels DIRECTLY in parallel, instead of
			// scanning the top-100 game directory and intersecting. The
			// directory truncation regularly misses small partner streamers.
			// This mirrors TDM core/client.py CHANNELS_FETCH for ACL channels.
			logins := make([]string, 0, len(c.Channels))
			loginToAllowed := make(map[string]twitch.DropChannel, len(c.Channels))
			for _, ch := range c.Channels {
				name := ch.Name
				if name == "" {
					continue
				}
				ll := strings.ToLower(name)
				logins = append(logins, ll)
				loginToAllowed[ll] = ch
			}
			infos := s.streams.GetChannelInfos(logins)
			for i, info := range infos {
				if info == nil || !info.IsLive {
					continue
				}
				// Strict: must actually be streaming the campaign's game
				if !strings.EqualFold(info.GameName, c.GameName) {
					continue
				}
				login := logins[i]
				entry, exists := byChannel[info.ID]
				if !exists {
					display := info.DisplayName
					if display == "" {
						if a, ok := loginToAllowed[login]; ok && a.DisplayName != "" {
							display = a.DisplayName
						} else {
							display = login
						}
					}
					entry = &PoolEntry{
						ChannelID:    info.ID,
						ChannelLogin: login,
						DisplayName:  display,
						ViewerCount:  info.ViewerCount,
					}
					byChannel[info.ID] = entry
				}
				entry.Campaigns = append(entry.Campaigns, ref)
			}
			continue
		}

		// No allow list — fall back to game-directory drops-enabled streams.
		streams := getDir(c.GameName)
		for _, st := range streams {
			login := strings.ToLower(st.BroadcasterLogin)
			entry, exists := byChannel[st.BroadcasterID]
			if !exists {
				entry = &PoolEntry{
					ChannelID:    st.BroadcasterID,
					ChannelLogin: login,
					DisplayName:  st.DisplayName,
					ViewerCount:  st.ViewerCount,
				}
				byChannel[st.BroadcasterID] = entry
			}
			entry.Campaigns = append(entry.Campaigns, ref)
		}
	}

	// Convert to slice
	pool := make([]*PoolEntry, 0, len(byChannel))
	for _, e := range byChannel {
		pool = append(pool, e)
	}
	return pool
}

// sortPool sorts entries in priority order:
//   1. wanted_games rank (lower index = higher priority; channels not in wanted go to end)
//   2. earliest endAt across the channel's campaigns
//   3. viewer count desc (tie-break)
//
// Empty wanted_games falls back to the v1.7.0 (endAt, viewers) ordering — fully
// backward compatible. Pin (v1.7.0 PinnedCampaignID) is silently ignored in v1.8.0.
func (s *DropSelector) sortPool(pool []*PoolEntry) {
	wanted := s.cfg.GetGamesToWatch()
	gameRanks := make(map[string]int, len(wanted))
	for i, g := range wanted {
		gameRanks[strings.ToLower(strings.TrimSpace(g))] = i
	}
	useGameSort := len(wanted) > 0
	notWantedRank := len(wanted)

	type cached struct {
		gameRank int
		minEnd   time.Time
	}
	keys := make(map[*PoolEntry]cached, len(pool))
	for _, e := range pool {
		var c cached
		c.gameRank = notWantedRank
		first := true
		for _, ref := range e.Campaigns {
			if useGameSort {
				if r, ok := gameRanks[strings.ToLower(ref.GameName)]; ok && r < c.gameRank {
					c.gameRank = r
				}
			}
			if first || ref.EndAt.Before(c.minEnd) {
				c.minEnd = ref.EndAt
				first = false
			}
		}
		keys[e] = c
	}

	sort.SliceStable(pool, func(i, j int) bool {
		ki, kj := keys[pool[i]], keys[pool[j]]
		if useGameSort && ki.gameRank != kj.gameRank {
			return ki.gameRank < kj.gameRank
		}
		if !ki.minEnd.Equal(kj.minEnd) {
			return ki.minEnd.Before(kj.minEnd)
		}
		return pool[i].ViewerCount > pool[j].ViewerCount
	})

	// Reorder each entry's Campaigns list by endAt only (pin support removed in v1.8.0).
	for _, e := range pool {
		sort.SliceStable(e.Campaigns, func(i, j int) bool {
			return e.Campaigns[i].EndAt.Before(e.Campaigns[j].EndAt)
		})
	}
}

// Select runs the full pipeline: filter → buildPool → sort → pick.
// Returns (pickedChannel, sortedPool). pickedChannel is nil if pool empty.
// skipChannels (channelID → true) are removed from the pool entirely — used
// for stall-cooldown so a channel that wasn't crediting drops doesn't keep
// getting re-picked. Pass nil if no skip set.
// The returned pool is sorted; callers can use pool[1:] as the queue for UI.
func (s *DropSelector) Select(campaigns []twitch.DropCampaign, skipChannels map[string]bool) (*PoolEntry, []*PoolEntry) {
	eligible := s.filterEligibleCampaigns(campaigns)
	if len(eligible) == 0 {
		return nil, nil
	}
	pool := s.buildPool(eligible)
	if len(pool) == 0 {
		return nil, nil
	}
	if len(skipChannels) > 0 {
		filtered := pool[:0]
		for _, e := range pool {
			if !skipChannels[e.ChannelID] {
				filtered = append(filtered, e)
			}
		}
		pool = filtered
		if len(pool) == 0 {
			return nil, nil
		}
	}
	s.sortPool(pool)
	return pool[0], pool
}
