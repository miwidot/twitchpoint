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

// keep imports used; sort is used by later additions.
var _ = sort.SliceStable
