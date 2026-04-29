package drops

import (
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
	Status             string    `json:"status"`               // ACTIVE / QUEUED / IDLE / DISABLED / COMPLETED
	IsPinned           bool      `json:"is_pinned"`
	QueueIndex         int       `json:"queue_index"`          // 1-based for ACTIVE/QUEUED/IDLE; 0 otherwise
	EtaMinutes         int       `json:"eta_minutes"`          // RequiredMinutesWatched - CurrentMinutesWatched of next-to-claim drop
}

// RowsConfig is the slice of config behavior BuildRows depends on.
// *config.Config satisfies this in production; tests can stub it.
type RowsConfig interface {
	GetPinnedCampaign() string
	IsCampaignDisabled(campaignID string) bool
	IsCampaignCompleted(campaignID string) bool
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
		if !c.IsAccountConnected {
			continue
		}

		// Skip campaigns with no watchable drops (sub-only, or all drops claimed).
		// These can't be farmed, so showing them in the queue is just noise.
		// EXCEPTION: keep them if disabled or completed so the user can see why.
		hasWatchable := false
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched > 0 && !d.IsClaimed {
				hasWatchable = true
				break
			}
		}
		if !hasWatchable && !cfg.IsCampaignDisabled(c.ID) && !cfg.IsCampaignCompleted(c.ID) {
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

	pct := 0
	if required > 0 {
		pct = (progress * 100) / required
		if pct > 100 {
			pct = 100
		}
	}

	eta := required - progress
	if eta < 0 {
		eta = 0
	}

	return ActiveDrop{
		CampaignID:         c.ID,
		CampaignName:       c.Name,
		GameName:           c.GameName,
		DropName:           dropName,
		Progress:           progress,
		Required:           required,
		Percent:            pct,
		EndAt:              c.EndAt,
		IsEnabled:          true,
		IsAccountConnected: c.IsAccountConnected,
		IsPinned:           c.ID == pinnedID && pinnedID != "",
		EtaMinutes:         eta,
	}
}
