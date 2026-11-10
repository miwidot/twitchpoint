package drops

import "github.com/miwi/twitchpoint/internal/twitch"

// RewardFilterConfig is the slice of config the reward-type filters need.
// *config.Config satisfies it.
//
// badge_emote_last is deliberately NOT part of this interface: it changes
// pool ORDER, not which campaigns are filtered out, and sortPool reads it
// straight off the concrete config. Keeping it out means an implementation
// of this interface only has to answer what rewardSkipReason actually asks.
type RewardFilterConfig interface {
	GetSkipBadgeEmote() bool
	GetSkipSubRewards() bool
}

// Skip reasons returned by rewardSkipReason ("" = keep the campaign).
const (
	skipBadgeEmote = "badge/emote only"
	skipSubReward  = "subscription reward"
)

// isBadgeEmoteOnly reports whether every benefit in the campaign is a BADGE
// or EMOTE. Campaigns that mix in-game items with a badge are NOT badge-only
// and are kept, as are campaigns with unknown benefit types (fail open).
func isBadgeEmoteOnly(c twitch.DropCampaign) bool {
	seen := false
	for _, d := range c.Drops {
		switch d.BenefitType {
		case "BADGE", "EMOTE":
			seen = true
		default:
			return false
		}
	}
	return seen
}

// hasSubGatedReward reports whether the campaign contains a reward that needs
// a subscription (requiredSubs > 0).
func hasSubGatedReward(c twitch.DropCampaign) bool {
	if c.HasSubReward {
		return true
	}
	for _, d := range c.Drops {
		if d.RequiredSubs > 0 {
			return true
		}
	}
	return false
}

// rewardSkipReason applies the user's reward-type filters. Shared by the
// selector and BuildRows so the UI and the picker never disagree.
func rewardSkipReason(cfg RewardFilterConfig, c twitch.DropCampaign) string {
	if cfg.GetSkipBadgeEmote() && isBadgeEmoteOnly(c) {
		return skipBadgeEmote
	}
	if cfg.GetSkipSubRewards() && hasSubGatedReward(c) {
		return skipSubReward
	}
	return ""
}
