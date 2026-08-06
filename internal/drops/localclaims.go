package drops

import (
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// claimRecordMaxAge is how long a claim stays in the record. Generous on
// purpose: it only has to outlive the campaign the drop belongs to, and a
// stale entry costs a few bytes while a missing one costs hours of pointless
// farming. Long-running campaigns re-register themselves every cycle for as
// long as Twitch keeps reporting them, so they never age out while alive.
const claimRecordMaxAge = 90 * 24 * time.Hour

// Local claim record — why this exists
//
// Twitch is only a reliable source for "did I already get this drop?" while
// the campaign sits in the in-progress inventory. Once every drop is claimed,
// Twitch removes the campaign from that list, and the dashboard/details
// payload that takes over carries NO per-user state at all: every drop comes
// back with isClaimed=false and currentMinutesWatched=0. A fully farmed
// campaign is then byte-for-byte identical to one that was never touched.
//
// applyClaimedBenefitFallback (internal/twitch) repairs that from the
// gameEventDrops award history, but it cannot help TIERED campaigns whose
// drops share a single benefit ID — MarbleFest, R6S "Esports Pack". There one
// award cannot say WHICH tier was earned, so guard 3 deliberately discards the
// signal rather than risk marking an unearned tier as claimed.
//
// That leaves a real gap, observed on 2026-07-28: MarbleFest Day1 was fully
// claimed at 07:50, dropped out of the inventory by 08:05, looked untouched
// again, and was re-farmed until it expired at 13:59 — roughly six hours of
// channel rotation in which Twitch credited not a single minute (the stall
// detector kept parking channels with "no credit — progress stuck at 0/0").
//
// The fix is to stop asking Twitch a question we can answer ourselves: we do
// the claiming, so we know what we got. The record is keyed by drop ID, which
// is unique per campaign instance — daily campaigns mint fresh IDs, so a new
// day is never mistaken for an old one.

// claimRecord is the slice of config behaviour the overlay needs.
// *config.Config satisfies this; tests can stub it.
type claimRecord interface {
	IsDropClaimed(dropID string) bool
	// RecordClaimedDrops liefert die Kennungen zurück, die NEU waren —
	// damit je Drop genau ein Eintrag in der Erfolgsliste entsteht.
	RecordClaimedDrops(dropIDs []string) []string
}

// applyLocalClaims marks drops as claimed when our own record says we already
// have them. Returns the number of drops corrected.
//
// Runs BEFORE auto-claim and the selector, so every later stage reasons about
// corrected data: auto-claim doesn't try to re-claim, the selector sees no
// earnable drop left, and the campaign is marked completed instead of farmed.
//
// Only ever flips false → true. A drop Twitch reports as claimed is never
// un-claimed by this, and a drop missing from the record is left exactly as
// Twitch describes it.
func applyLocalClaims(campaigns []twitch.DropCampaign, rec claimRecord) int {
	if rec == nil {
		return 0
	}
	corrected := 0
	for i := range campaigns {
		for j := range campaigns[i].Drops {
			d := &campaigns[i].Drops[j]
			if d.IsClaimed || d.ID == "" {
				continue
			}
			if rec.IsDropClaimed(d.ID) {
				d.IsClaimed = true
				corrected++
			}
		}
	}
	return corrected
}

// recordObservedClaims persists every drop that is currently marked claimed
// and returns one history entry per NEWLY recorded drop, ready to append to the
// Erfolgsliste. An empty result means nothing changed — no Save needed.
//
// Runs AFTER auto-claim so it also captures drops claimed in this very cycle.
// That matters for exactly the failure case above: the last tier was claimed
// at 07:50 and the campaign had left the inventory by the next fetch — waiting
// for the next cycle to observe it would have missed it for good.
//
// justClaimed carries the IDs auto-claim got in this cycle, so the history can
// tell "selbst geclaimt" from "war schon geclaimt" (der Nutzer hat es bei
// Twitch selbst abgeholt). Rein informativ.
func recordObservedClaims(campaigns []twitch.DropCampaign, rec claimRecord, justClaimed map[string]bool) []ClaimHistoryEntry {
	if rec == nil {
		return nil
	}
	// Namen zur Kennung mitnehmen: nach Ablauf der Kampagne sind sie bei
	// Twitch nicht mehr zu holen, die Kennung allein wäre wertlos.
	type meta struct{ game, campaign, reward string }
	info := make(map[string]meta)
	var ids []string

	for i := range campaigns {
		c := &campaigns[i]
		for j := range c.Drops {
			d := &c.Drops[j]
			if !d.IsClaimed || d.ID == "" {
				continue
			}
			ids = append(ids, d.ID)
			reward := d.BenefitName
			if reward == "" {
				reward = d.Name
			}
			info[d.ID] = meta{game: c.GameName, campaign: c.Name, reward: reward}
		}
	}

	added := rec.RecordClaimedDrops(ids)
	if len(added) == 0 {
		return nil
	}

	now := time.Now().UTC()
	entries := make([]ClaimHistoryEntry, 0, len(added))
	for _, id := range added {
		m := info[id]
		source := "extern"
		if justClaimed[id] {
			source = "auto"
		}
		entries = append(entries, ClaimHistoryEntry{
			At:       now,
			Game:     m.game,
			Campaign: m.campaign,
			Reward:   m.reward,
			DropID:   id,
			Source:   source,
		})
	}
	return entries
}

// claimedCounts reports how many of a campaign's watchable drops are claimed.
// Drops without a watch-time requirement are not counted at all — they can't
// be farmed (sub-gated or reward-only tiers), so including them would make a
// finished campaign look unfinished in the UI.
func claimedCounts(c twitch.DropCampaign) (claimed, watchable int) {
	for _, d := range c.Drops {
		if d.RequiredMinutesWatched <= 0 {
			continue
		}
		watchable++
		if d.IsClaimed {
			claimed++
		}
	}
	return claimed, watchable
}
