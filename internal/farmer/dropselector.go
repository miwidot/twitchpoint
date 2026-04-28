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
			continue // can't query directory without game name
		}

		streams := getDir(c.GameName)
		if len(streams) == 0 {
			continue
		}

		// Build allowed-channel lookup if campaign has restrictions
		var allowedByID map[string]bool
		var allowedByLogin map[string]bool
		hasAllow := len(c.Channels) > 0
		if hasAllow {
			allowedByID = make(map[string]bool, len(c.Channels))
			allowedByLogin = make(map[string]bool, len(c.Channels))
			for _, ch := range c.Channels {
				if ch.ID != "" {
					allowedByID[ch.ID] = true
				}
				if ch.Name != "" {
					allowedByLogin[strings.ToLower(ch.Name)] = true
				}
				if ch.DisplayName != "" {
					allowedByLogin[strings.ToLower(ch.DisplayName)] = true
				}
			}
		}

		for _, st := range streams {
			login := strings.ToLower(st.BroadcasterLogin)

			if hasAllow {
				// Skip if not in allow list
				if !allowedByID[st.BroadcasterID] && !allowedByLogin[login] {
					continue
				}
			}

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
//   1. Channels serving any pinned campaign first
//   2. Then by earliest endAt across the channel's campaigns (closest expiry wins)
//   3. Then by viewer count desc (stability tie-break, mild preference for popular streams)
func (s *DropSelector) sortPool(pool []*PoolEntry) {
	// Pre-compute earliest end and pin status per entry for stable comparison
	type cached struct {
		hasPinned bool
		minEnd    time.Time
	}
	keys := make(map[*PoolEntry]cached, len(pool))
	for _, e := range pool {
		var c cached
		first := true
		for _, ref := range e.Campaigns {
			if ref.IsPinned {
				c.hasPinned = true
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
		// Pinned first
		if ki.hasPinned != kj.hasPinned {
			return ki.hasPinned
		}
		// Earlier endAt first
		if !ki.minEnd.Equal(kj.minEnd) {
			return ki.minEnd.Before(kj.minEnd)
		}
		// Higher viewers first
		return pool[i].ViewerCount > pool[j].ViewerCount
	})

	// Also reorder each entry's own Campaigns list by (pinned, endAt) so
	// callers can index Campaigns[0] for "primary campaign of this channel".
	for _, e := range pool {
		sort.SliceStable(e.Campaigns, func(i, j int) bool {
			a, b := e.Campaigns[i], e.Campaigns[j]
			if a.IsPinned != b.IsPinned {
				return a.IsPinned
			}
			return a.EndAt.Before(b.EndAt)
		})
	}
}

// Select runs the full pipeline: filter → buildPool → sort → pick.
// Returns (pickedChannel, sortedPool). pickedChannel is nil if pool empty.
// The returned pool is sorted; callers can use pool[1:] as the queue for UI.
func (s *DropSelector) Select(campaigns []twitch.DropCampaign) (*PoolEntry, []*PoolEntry) {
	eligible := s.filterEligibleCampaigns(campaigns)
	if len(eligible) == 0 {
		return nil, nil
	}
	pool := s.buildPool(eligible)
	if len(pool) == 0 {
		return nil, nil
	}
	s.sortPool(pool)
	return pool[0], pool
}
