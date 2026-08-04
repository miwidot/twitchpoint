package drops

import (
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// ActiveDrop represents a drop being tracked, exposed for the Web UI.
// JSON tags here are part of the public /api/drops contract — renaming a
// field means a coordinated UI bump.
type ActiveDrop struct {
	CampaignID         string    `json:"campaign_id"`
	CampaignName       string    `json:"campaign_name"`
	GameName           string    `json:"game_name"`
	DropName           string    `json:"drop_name"`
	ChannelLogin       string    `json:"channel_login"`        // matched channel (if any)
	Progress           int       `json:"progress"`             // current minutes watched
	Required           int       `json:"required"`             // minutes required
	Percent            int       `json:"percent"`              // 0-100
	IsClaimed          bool      `json:"is_claimed"`
	EndAt              time.Time `json:"end_at"`               // campaign end time
	IsAutoSelected     bool      `json:"is_auto_selected"`     // channel was auto-discovered
	IsEnabled          bool      `json:"is_enabled"`           // campaign not disabled
	IsAccountConnected bool      `json:"is_account_connected"` // account linked for this game
	// IsAutoDiscovered marks campaigns whose GameName is NOT in the
	// user's wanted_games priority list — i.e. the bot is farming them
	// automatically because the account is linked, not because the
	// user explicitly requested the game. Only set when wanted_games
	// is non-empty (with an empty list, ALL eligible campaigns are
	// auto-discovered and the marker would be noise).
	IsAutoDiscovered bool `json:"is_auto_discovered"`
	Status             string    `json:"status"`               // ACTIVE / QUEUED / IDLE / DISABLED / COMPLETED / BLOCKED
	// BlockReason explains a BLOCKED (or IDLE) status in one short phrase
	// so the UI can show WHY a wanted-game campaign isn't being farmed.
	BlockReason        string    `json:"block_reason,omitempty"`
	IsPinned           bool      `json:"is_pinned"`
	QueueIndex         int       `json:"queue_index"`          // 1-based for ACTIVE/QUEUED/IDLE; 0 otherwise
	EtaMinutes         int       `json:"eta_minutes"`          // RequiredMinutesWatched - CurrentMinutesWatched of next-to-claim drop
	// ClaimedDrops / WatchableDrops show how far a campaign has come, e.g.
	// "3/5". Only drops with a watch-time requirement are counted — sub-gated
	// or reward-only tiers can't be farmed, so counting them would make a
	// finished campaign look unfinished. Added because a tiered campaign that
	// is fully claimed can otherwise look identical to an untouched one, which
	// is exactly what made the MarbleFest re-farming bug invisible. (lokal)
	ClaimedDrops   int `json:"claimed_drops"`
	WatchableDrops int `json:"watchable_drops"`
}

// RowsConfig is the slice of config behavior BuildRows depends on.
// *config.Config satisfies this in production; tests can stub it.
type RowsConfig interface {
	GetPinnedCampaign() string
	IsCampaignDisabled(campaignID string) bool
	IsCampaignCompleted(campaignID string) bool
	GetGamesToWatch() []string
}

// BuildRows produces the per-campaign UI rows for the web API. It
// classifies each campaign as ACTIVE (matches the current pick), QUEUED
// (in the selector pool but not picked), IDLE (no live channels right
// now), DISABLED (user-disabled), or COMPLETED (config flag set).
//
// Sub-only-deduped campaigns (no watchable drops) are silently skipped
// unless the user explicitly disabled or completed them — keeping them
// visible in those cases makes the reason discoverable.
func BuildRows(
	cfg RowsConfig,
	campaigns []twitch.DropCampaign,
	pick *PoolEntry,
	pool []*PoolEntry,
) (active, queued, idle []ActiveDrop) {
	pinnedID := cfg.GetPinnedCampaign()

	// Build a lower-cased lookup of wanted-games. Auto-discovered marker
	// is meaningful ONLY when this list is non-empty — when it's empty,
	// EVERY eligible campaign is auto-discovered and tagging them all
	// would be noise.
	wanted := cfg.GetGamesToWatch()
	wantedSet := make(map[string]bool, len(wanted))
	for _, g := range wanted {
		wantedSet[strings.ToLower(strings.TrimSpace(g))] = true
	}
	useAutoMarker := len(wantedSet) > 0

	campaignsInPool := make(map[string]*PoolEntry)
	for _, e := range pool {
		for _, ref := range e.Campaigns {
			if _, exists := campaignsInPool[ref.ID]; !exists {
				campaignsInPool[ref.ID] = e
			}
		}
	}

	pickedCampaignIDs := make(map[string]bool)
	if pick != nil {
		for _, ref := range pick.Campaigns {
			pickedCampaignIDs[ref.ID] = true
		}
	}

	queueIdx := 1
	seenWatchableNames := make(map[string]bool) // dedup sub-only-deduped campaign noise (e.g. 9× "S5 Support ABI Partners")
	for _, c := range campaigns {
		if c.Status != "" && c.Status != "ACTIVE" {
			continue
		}
		if !c.EndAt.IsZero() && !c.EndAt.After(time.Now()) {
			continue
		}
		// wanted_games strict whitelist — same gate as the Selector.
		// When the user has explicit priority games set, the UI shouldn't
		// surface campaigns from other games (they're not farmable anyway
		// per the strict filter, so listing them is noise).
		if useAutoMarker && !wantedSet[strings.ToLower(strings.TrimSpace(c.GameName))] {
			continue
		}

		// Campaigns with no watchable drops (sub-only, or all drops claimed)
		// can't be farmed. Needed by the block classification below.
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched > 0 && !d.IsClaimed {
				hasWatchable = true
				break
			}
		}

		// A wanted-game campaign that cannot be farmed is now surfaced
		// instead of silently dropped, so the user can see WHY. Both gates
		// (eligibility — same parity as Selector.filterEligibleCampaigns —
		// and "nothing watchable") feed one classification:
		//
		//   BLOCKED = the user has to act (game account not linked). Shown
		//             prominently, because this silently costs drops.
		//   IDLE    = harmless: every drop already claimed, or nothing in
		//             its time window right now. No action needed.
		//
		// Campaigns of games that are NOT in the wanted list keep being
		// hidden — surfacing those would just be noise. Disabled and
		// completed campaigns keep their own status.
		blockStatus, blockReason := "", ""
		if !c.IsAccountConnected && !hasBadgeOrEmoteBenefit(c) {
			blockStatus, blockReason = "BLOCKED", "account not linked"
		} else if !hasWatchable {
			blockStatus, blockReason = "IDLE", "nothing open"
		}
		if blockStatus != "" && !cfg.IsCampaignDisabled(c.ID) && !cfg.IsCampaignCompleted(c.ID) {
			if !useAutoMarker {
				// No wanted list configured — keep the previous behaviour
				// of hiding non-farmable campaigns entirely.
				continue
			}
			if seenWatchableNames[c.Name] {
				continue
			}
			seenWatchableNames[c.Name] = true
			row := campaignToRow(c, pinnedID)
			row.Status = blockStatus
			row.BlockReason = blockReason
			idle = append(idle, row)
			continue
		}

		// Dedup by name: when Twitch returns N copies of the same campaign with
		// different IDs (each with one allowed channel — typical for streamer-
		// exclusive drops), show only the first. The selector still considers
		// all of them; this is purely a UI dedup.
		if seenWatchableNames[c.Name] {
			continue
		}
		seenWatchableNames[c.Name] = true

		row := campaignToRow(c, pinnedID)
		if useAutoMarker && !wantedSet[strings.ToLower(strings.TrimSpace(c.GameName))] {
			row.IsAutoDiscovered = true
		}

		switch {
		case cfg.IsCampaignDisabled(c.ID):
			row.Status = "DISABLED"
			active = append(active, row)
		case cfg.IsCampaignCompleted(c.ID):
			row.Status = "COMPLETED"
			active = append(active, row)
		case pickedCampaignIDs[c.ID]:
			row.Status = "ACTIVE"
			row.QueueIndex = queueIdx
			queueIdx++
			if pick != nil {
				row.ChannelLogin = pick.ChannelLogin
			}
			active = append(active, row)
		case campaignsInPool[c.ID] != nil:
			row.Status = "QUEUED"
			row.QueueIndex = queueIdx
			queueIdx++
			queued = append(queued, row)
		default:
			row.Status = "IDLE"
			idle = append(idle, row)
		}
	}

	return active, queued, idle
}

// campaignToRow projects a DropCampaign into the ActiveDrop UI shape.
// Status / QueueIndex / ChannelLogin are filled in by BuildRows after
// it decides the row's bucket.
func campaignToRow(c twitch.DropCampaign, pinnedID string) ActiveDrop {
	var dropName string
	var progress, required int
	for _, d := range c.Drops {
		if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
			continue
		}
		dropName = d.BenefitName
		if dropName == "" {
			dropName = d.Name
		}
		progress = d.CurrentMinutesWatched
		required = d.RequiredMinutesWatched
		break
	}

	claimed, watchable := claimedCounts(c) // lokal: "x/y erhalten"-Anzeige

	row := ActiveDrop{
		CampaignID:         c.ID,
		CampaignName:       c.Name,
		GameName:           c.GameName,
		DropName:           dropName,
		Progress:           progress,
		Required:           required,
		EndAt:              c.EndAt,
		IsEnabled:          true,
		IsAccountConnected: c.IsAccountConnected,
		IsPinned:           c.ID == pinnedID && pinnedID != "",
		ClaimedDrops:       claimed,
		WatchableDrops:     watchable,
	}
	row.recomputeDerived()
	return row
}

// recomputeDerived recalculates Percent and EtaMinutes from Progress and
// Required. Both are pure functions of those two fields, so storing them
// independently invites divergence: a progress event that advances
// Progress without recomputing Percent/EtaMinutes (e.g. when Required was
// momentarily 0) leaves the row showing a stale "248/300min (88%)" where
// the percentage no longer matches the minutes. Every writer of Progress
// or Required MUST call this before the row is published so the three
// fields can never disagree on screen.
func (d *ActiveDrop) recomputeDerived() {
	if d.Required <= 0 {
		d.Percent = 0
		d.EtaMinutes = 0
		return
	}
	pct := (d.Progress * 100) / d.Required
	if pct > 100 {
		pct = 100
	}
	d.Percent = pct
	eta := d.Required - d.Progress
	if eta < 0 {
		eta = 0
	}
	d.EtaMinutes = eta
}
