package drops

import (
	"sync"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// StallCooldownDuration is how long a channel is excluded from the pool
// after Twitch failed to credit drop progress for that channel for one
// cycle. 30 min ≈ 6 cycles — long enough that we exhaust other candidates
// before retrying, short enough to recover from temporary Twitch hiccups.
const StallCooldownDuration = 30 * time.Minute

// CooldownReason explains why a channel is currently in cooldown. It
// matters because the stall-recovery path clears cooldowns when a channel
// credits new minutes — but that recovery must NOT clear cooldowns set
// deliberately by other paths (game change, ID mismatch) that have their
// own expiry.
type CooldownReason int

const (
	// CooldownStall is set when Apply() detects no minutes credited in
	// the previous cycle. Clearable on later credit.
	CooldownStall CooldownReason = iota
	// CooldownManual is set deliberately by callers (game change,
	// id-mismatch). NOT clearable by credit recovery — only the timeout
	// removes it.
	CooldownManual
)

type cooldownEntry struct {
	expires time.Time
	reason  CooldownReason
}

// StallTracker tracks Twitch's drop-credit reliability per channel.
// It snapshots the picked channel/campaign/progress at the end of each
// inventory cycle, then compares the next cycle's progress to that
// baseline. If progress did not advance, the channel goes into stall
// cooldown so the selector skips it. It also accepts manual cooldowns
// from game-change and id-mismatch paths that must NOT be cleared by
// credit recovery.
//
// All methods are safe for concurrent use; the tracker owns its own
// mutex and never reaches into Farmer state.
type StallTracker struct {
	mu       sync.Mutex
	cooldown map[string]cooldownEntry
	log      func(string, ...interface{})

	// Baseline for the next Apply() comparison.
	lastPickChannelID  string
	lastPickCampaignID string
	lastPickProgress   int
}

// NewStallTracker constructs a StallTracker. log may be nil; if non-nil
// it receives the "no credit on X" line whenever Apply records a stall.
func NewStallTracker(log func(string, ...interface{})) *StallTracker {
	return &StallTracker{
		cooldown: make(map[string]cooldownEntry),
		log:      log,
	}
}

// SnapshotPick records the picked channel's primary-campaign progress
// so the next Apply() can compare. Pass nil pick to clear the baseline.
func (s *StallTracker) SnapshotPick(pick *PoolEntry, campaigns []twitch.DropCampaign) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pick == nil || len(pick.Campaigns) == 0 {
		s.lastPickChannelID = ""
		s.lastPickCampaignID = ""
		s.lastPickProgress = 0
		return
	}
	primaryCampID := pick.Campaigns[0].ID
	progress := 0
	for _, c := range campaigns {
		if c.ID != primaryCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 || d.IsClaimed {
				continue
			}
			progress = d.CurrentMinutesWatched
			break
		}
		break
	}
	s.lastPickChannelID = pick.ChannelID
	s.lastPickCampaignID = primaryCampID
	s.lastPickProgress = progress
}

// Apply compares the snapshot against the new inventory. If the
// previously snapshotted pick's progress did not advance, the channel
// gets a stall-reason cooldown. If progress advanced, ONLY the
// stall-reason cooldown is cleared — manual cooldowns are preserved.
func (s *StallTracker) Apply(campaigns []twitch.DropCampaign) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prevChID := s.lastPickChannelID
	prevCampID := s.lastPickCampaignID
	prevProgress := s.lastPickProgress
	if prevChID == "" || prevCampID == "" {
		return // no previous pick to evaluate
	}

	// Find the previous pick's drop progress in the new inventory.
	currentProgress := -1
	for _, c := range campaigns {
		if c.ID != prevCampID {
			continue
		}
		for _, d := range c.Drops {
			if d.RequiredMinutesWatched <= 0 {
				continue
			}
			if d.IsClaimed {
				continue
			}
			currentProgress = d.CurrentMinutesWatched
			break
		}
		break
	}

	if currentProgress < 0 {
		// Campaign disappeared from inventory or fully claimed. Either
		// way, no stall to record.
		return
	}

	if currentProgress > prevProgress {
		// Twitch credited at least one minute — channel is healthy.
		// Clear ONLY a stall-reason cooldown. Manual cooldowns
		// (game-change, id-mismatch) must run their own timer so
		// user-deliberate skips aren't undone by a single credited
		// minute.
		if cd, ok := s.cooldown[prevChID]; ok && cd.reason == CooldownStall {
			delete(s.cooldown, prevChID)
		}
		return
	}

	// No credit since last cycle — record a stall-reason cooldown.
	s.cooldown[prevChID] = cooldownEntry{
		expires: time.Now().Add(StallCooldownDuration),
		reason:  CooldownStall,
	}
	if s.log != nil {
		s.log("[Drops/Pool] no credit on %s (progress stuck at %d/%d) — %v cooldown",
			prevChID, currentProgress, prevProgress, StallCooldownDuration)
	}
}

// SetManual records a manual-reason cooldown that won't be auto-cleared
// by progress recovery — only the timeout removes it. Used by callers
// that deliberately want a channel skipped (game change, id mismatch).
func (s *StallTracker) SetManual(channelID string, dur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cooldown[channelID] = cooldownEntry{
		expires: time.Now().Add(dur),
		reason:  CooldownManual,
	}
}

// ActiveSkipSet returns the set of channelIDs currently in cooldown.
// Expired entries are pruned from the underlying map as a side effect.
func (s *StallTracker) ActiveSkipSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	skip := make(map[string]bool, len(s.cooldown))
	now := time.Now()
	for chID, cd := range s.cooldown {
		if now.Before(cd.expires) {
			skip[chID] = true
		} else {
			delete(s.cooldown, chID)
		}
	}
	return skip
}

// LastPickCampaignID returns the campaign ID of the most recently
// snapshotted pick (or "" if none). Used by callers that need to know
// which campaign the picked channel was supposed to serve — e.g., the
// game-change handler looks up the expected game from this campaign's
// cached metadata, and the progress poller uses it to scope its GQL
// query.
func (s *StallTracker) LastPickCampaignID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPickCampaignID
}
