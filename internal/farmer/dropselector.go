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

// keep imports used; sort/strings are used by later additions.
var _ = sort.SliceStable
var _ = strings.ToLower
