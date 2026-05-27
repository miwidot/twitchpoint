package drops

import (
	"sort"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// streamSource is the minimal GQL interface the selector needs. Mocked in tests.
type streamSource interface {
	GetGameStreamsDropsEnabled(slug string, limit int) ([]twitch.GameStream, error)
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

// FilterStats counts rejection reasons from the most recent filter pass.
// Surfaced by Select callers to diagnose "empty pool" symptoms — without
// these numbers it's impossible to tell whether 0 candidates means
// "no eligible campaigns" or "eligible but no live streamers".
type FilterStats struct {
	Total           int
	StatusRejected  int // non-ACTIVE status
	Expired         int // EndAt in the past
	NotInWanted     int // wanted_games is non-empty AND campaign's game not in it
	NotConnected    int // isAccountConnected=false AND no badge/emote benefit
	Disabled        int // user-disabled
	Completed       int // user-marked completed
	NoEarnableDrops int // no IsEarnable drop right now (claimed / out-of-window / precondition gated)
	Eligible        int
}

// Selector is a pure pick-a-channel function with no side effects on Farmer state.
// Construction takes everything it needs as dependencies so tests can substitute mocks.
type Selector struct {
	cfg          *config.Config
	streams      streamSource
	now          func() time.Time // injectable for deterministic tests
	lastFilter   FilterStats      // populated by every Select(); LastFilterStats() reads it
	lastPoolSize int              // candidates after buildPool, before skip-set
	diagFn       func(format string, args ...interface{})
}

// SetDiagSink wires a logger for filter-reject diagnostics. Without it,
// rejected-wanted-game logging is silently dropped.
func (s *Selector) SetDiagSink(fn func(format string, args ...interface{})) {
	s.diagFn = fn
}

// NewSelector constructs a selector with the production stream source.
func NewSelector(cfg *config.Config, gql *twitch.GQLClient) *Selector {
	return &Selector{
		cfg:     cfg,
		streams: gql,
		now:     time.Now,
	}
}

// filterEligibleCampaigns drops campaigns that are not currently farmable:
// non-active status, expired, account not connected, disabled by user,
// already completed, or have no watchable (non-sub-only, non-claimed) drops.
func (s *Selector) filterEligibleCampaigns(campaigns []twitch.DropCampaign) []twitch.DropCampaign {
	now := s.now()
	out := make([]twitch.DropCampaign, 0, len(campaigns))
	stats := FilterStats{Total: len(campaigns)}

	// wanted_games as strict whitelist. Empty list = no restriction; any
	// non-empty list excludes everything else.
	// Without this, badge/emote campaigns from random Twitch-side games
	// (TwitchCon, chat-badge promos, etc.) leak into the pool whenever the
	// user's priority games have no current pool entry — and end up picked
	// because sortPool only ranks, doesn't gate.
	wanted := s.cfg.GetGamesToWatch()
	wantedSet := make(map[string]bool, len(wanted))
	for _, g := range wanted {
		wantedSet[strings.ToLower(strings.TrimSpace(g))] = true
	}
	hasWantedFilter := len(wantedSet) > 0

	// One-shot diagnostic dump: for every campaign whose game IS in the wanted
	// list, log status/connection/benefit-type so we can see why a "should
	// work" campaign got rejected. Routes through diagLog (file logger) so
	// it's visible on Windows too.
	logWantedReject := func(c twitch.DropCampaign, reason string) {
		if !hasWantedFilter {
			return
		}
		if !wantedSet[strings.ToLower(strings.TrimSpace(c.GameName))] {
			return
		}
		benefitTypes := make([]string, 0, len(c.Drops))
		for _, d := range c.Drops {
			benefitTypes = append(benefitTypes, d.BenefitType)
		}
		if s.diagFn == nil {
			return
		}
		s.diagFn("[Drops/Diag] wanted-game campaign rejected (%s): name=%q game=%q status=%q connected=%t inInventory=%t drops=%d benefitTypes=%v allowChannels=%d endAt=%s",
			reason, c.Name, c.GameName, c.Status, c.IsAccountConnected, c.InInventory, len(c.Drops), benefitTypes, len(c.Channels), c.EndAt.Format(time.RFC3339))
	}

	// One-shot dump: enumerate EVERY campaign whose game is in the wanted list,
	// regardless of filter outcome. Lets us spot "missing campaign" cases
	// (e.g. Twitch dropped one between two cycles) that the reject-log can't
	// surface because the campaign isn't there to reject.
	if hasWantedFilter && s.diagFn != nil {
		count := 0
		for _, c := range campaigns {
			if !wantedSet[strings.ToLower(strings.TrimSpace(c.GameName))] {
				continue
			}
			count++
			s.diagFn("[Drops/Diag] wanted-game in candidates: name=%q game=%q status=%q connected=%t inInventory=%t drops=%d endAt=%s",
				c.Name, c.GameName, c.Status, c.IsAccountConnected, c.InInventory, len(c.Drops), c.EndAt.Format(time.RFC3339))
		}
		s.diagFn("[Drops/Diag] wanted-game total in candidates: %d", count)
	}

	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			logWantedReject(c, "status")
			stats.StatusRejected++
			continue
		}
		if !c.EndAt.IsZero() && !c.EndAt.After(now) {
			logWantedReject(c, "expired")
			stats.Expired++
			continue
		}
		if hasWantedFilter && !wantedSet[strings.ToLower(strings.TrimSpace(c.GameName))] {
			stats.NotInWanted++
			continue
		}
		// Account-link OR badge/emote eligibility. A campaign is earnable
		// without a linked publisher account if its benefit is a Twitch-side
		// reward (BADGE or EMOTE). Only DIRECT_ENTITLEMENT rewards (in-game
		// items) actually require a linked game account. Skipping this
		// branch filtered out the badge/emote campaigns that should still
		// be farmable.
		if !c.IsAccountConnected && !hasBadgeOrEmoteBenefit(c) {
			logWantedReject(c, "not_connected")
			stats.NotConnected++
			continue
		}
		if s.cfg.IsCampaignDisabled(c.ID) {
			stats.Disabled++
			continue
		}
		if s.cfg.IsCampaignCompleted(c.ID) {
			stats.Completed++
			continue
		}
		// Need at least one drop that's actually earnable RIGHT NOW.
		// IsEarnable mirrors TDM's TimedDrop._base_can_earn — checks
		// not just unclaimed-with-required-mins but also the per-drop
		// time window and the precondition chain. Without this, the
		// selector picks campaigns whose first-unclaimed drop is
		// gated (precondition unclaimed, or startAt in the future)
		// and Twitch refuses to credit, leaving the bot blind-
		// heartbeating until the silent-pick threshold trips.
		hasWatchable := false
		for _, d := range c.Drops {
			if d.IsEarnable(now, c.Drops) {
				hasWatchable = true
				break
			}
		}
		if !hasWatchable {
			logWantedReject(c, "no_earnable")
			stats.NoEarnableDrops++
			continue
		}
		out = append(out, c)
	}

	stats.Eligible = len(out)
	s.lastFilter = stats
	return out
}

// buildPool turns an eligible-campaign list into a deduped pool of candidate
// channels. For campaigns with an allow list, it queries the drops-enabled
// game directory and intersects with that list. For unrestricted campaigns,
// the top drops-enabled streams for the game become candidates directly.
//
// Channels appearing in multiple campaigns are deduped — a single PoolEntry
// carries all the campaigns it serves.
func (s *Selector) buildPool(eligible []twitch.DropCampaign) []*PoolEntry {
	pinnedID := s.cfg.GetPinnedCampaign()

	// Game-slug → cached directory result, so we hit GQL at most once per game per cycle.
	// Slug (URL form) is required by the persisted-hash GameDirectory query; if a campaign
	// didn't carry one, we derive it from the displayName as a fallback.
	dirCache := make(map[string][]twitch.GameStream)
	getDir := func(gameSlug, gameName string) []twitch.GameStream {
		slug := gameSlug
		if slug == "" {
			slug = twitch.SlugFromGameName(gameName)
		}
		if slug == "" {
			return nil
		}
		if cached, ok := dirCache[slug]; ok {
			return cached
		}
		streams, err := s.streams.GetGameStreamsDropsEnabled(slug, 100)
		if err != nil {
			dirCache[slug] = nil // negative cache for the cycle
			return nil
		}
		dirCache[slug] = streams
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
		streams := getDir(c.GameSlug, c.GameName)
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
func (s *Selector) sortPool(pool []*PoolEntry) {
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
func (s *Selector) Select(campaigns []twitch.DropCampaign, skipChannels map[string]bool) (*PoolEntry, []*PoolEntry) {
	eligible := s.filterEligibleCampaigns(campaigns)
	if len(eligible) == 0 {
		return nil, nil
	}
	pool := s.buildPool(eligible)
	s.lastPoolSize = len(pool)
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

// LastFilterStats returns the filter-stage rejection counts from the most
// recent Select call. Use to diagnose "empty pool" symptoms.
func (s *Selector) LastFilterStats() FilterStats { return s.lastFilter }

// hasBadgeOrEmoteBenefit reports whether any of the campaign's drops awards
// a BADGE or EMOTE — Twitch-side rewards that can be earned without linking
// a publisher account.
func hasBadgeOrEmoteBenefit(c twitch.DropCampaign) bool {
	for _, d := range c.Drops {
		switch d.BenefitType {
		case "BADGE", "EMOTE":
			return true
		}
	}
	return false
}

// LastPoolSize returns how many channel candidates the pool stage produced
// from the most recent Select. 0 with Eligible>0 means the filter passed
// campaigns but no live drops-enabled streamer was found for any of them.
func (s *Selector) LastPoolSize() int { return s.lastPoolSize }
