package drops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/twitch"
)

// fakeClaimRecord is an in-memory stand-in for the config's claim record.
type fakeClaimRecord struct {
	seen map[string]bool
}

func newFakeClaimRecord(ids ...string) *fakeClaimRecord {
	f := &fakeClaimRecord{seen: map[string]bool{}}
	for _, id := range ids {
		f.seen[id] = true
	}
	return f
}

func (f *fakeClaimRecord) IsDropClaimed(id string) bool { return f.seen[id] }

func (f *fakeClaimRecord) RecordClaimedDrops(ids []string) []string {
	var added []string
	for _, id := range ids {
		if id == "" || f.seen[id] {
			continue
		}
		f.seen[id] = true
		added = append(added, id)
	}
	return added
}

// TestApplyLocalClaims_TieredCampaignLeftInventory is the MarbleFest
// regression (2026-07-28): a fully claimed tiered daily campaign drops out of
// Twitch's in-progress inventory, the dashboard payload that replaces it
// reports every drop as unclaimed with zero progress, and the shared benefit ID
// makes the gameEventDrops fallback unusable. Without the local record the bot
// re-farmed the campaign for ~6 hours until it expired.
func TestApplyLocalClaims_TieredCampaignLeftInventory(t *testing.T) {
	// Exactly what the dashboard reports after the campaign left the inventory.
	camps := []twitch.DropCampaign{{
		ID:          "marblefest-day1",
		Name:        "MarbleFest - July'26-Day1",
		InInventory: false,
		Drops: []twitch.TimeBasedDrop{
			{ID: "d1", BenefitID: "coins", RequiredMinutesWatched: 120, CurrentMinutesWatched: 0},
			{ID: "d2", BenefitID: "coins", RequiredMinutesWatched: 240, CurrentMinutesWatched: 0},
			{ID: "d3", BenefitID: "coins", RequiredMinutesWatched: 360, CurrentMinutesWatched: 0},
			{ID: "d4", BenefitID: "coins", RequiredMinutesWatched: 480, CurrentMinutesWatched: 0},
			{ID: "d5", BenefitID: "coins", RequiredMinutesWatched: 900, CurrentMinutesWatched: 0},
		},
	}}
	rec := newFakeClaimRecord("d1", "d2", "d3", "d4", "d5")

	if got := applyLocalClaims(camps, rec); got != 5 {
		t.Fatalf("expected all 5 tiers corrected, got %d", got)
	}
	for _, d := range camps[0].Drops {
		if !d.IsClaimed {
			t.Fatalf("drop %s should be claimed from the local record", d.ID)
		}
	}
	// The point of the fix: nothing is earnable any more, so the selector
	// stops picking this campaign.
	for _, d := range camps[0].Drops {
		if d.IsEarnable(camps[0].EndAt, camps[0].Drops) {
			t.Fatalf("drop %s must not be earnable after the correction", d.ID)
		}
	}
}

// TestApplyLocalClaims_UnknownDropsUntouched: a drop we have no record of must
// stay exactly as Twitch describes it — the overlay only ever fills in what we
// know, it never invents claims. Guards against a fresh daily campaign being
// skipped because an older one was claimed.
func TestApplyLocalClaims_UnknownDropsUntouched(t *testing.T) {
	camps := []twitch.DropCampaign{{
		ID: "marblefest-day2",
		Drops: []twitch.TimeBasedDrop{
			{ID: "new1", RequiredMinutesWatched: 120},
			{ID: "new2", RequiredMinutesWatched: 240},
		},
	}}
	rec := newFakeClaimRecord("d1", "d2") // yesterday's IDs

	if got := applyLocalClaims(camps, rec); got != 0 {
		t.Fatalf("no drop should have been corrected, got %d", got)
	}
	for _, d := range camps[0].Drops {
		if d.IsClaimed {
			t.Fatalf("drop %s of a fresh campaign must stay unclaimed", d.ID)
		}
	}
}

// TestRecordObservedClaims_CapturesFreshClaims: the recorder runs after
// auto-claim, so a tier claimed in this very cycle must land in the record.
// Waiting for the next inventory fetch would lose it whenever the campaign
// leaves the inventory in between — which is precisely how the last MarbleFest
// tier slipped through.
func TestRecordObservedClaims_CapturesFreshClaims(t *testing.T) {
	camps := []twitch.DropCampaign{{
		ID:       "camp",
		Name:     "MarbleFest - July'26-Day2",
		GameName: "Marbles on Stream",
		Drops: []twitch.TimeBasedDrop{
			{ID: "old", RequiredMinutesWatched: 120, IsClaimed: true, BenefitName: "15 Community Coins"},
			{ID: "fresh", RequiredMinutesWatched: 900, IsClaimed: true, BenefitName: "30 Tournament Coins"},
			{ID: "open", RequiredMinutesWatched: 1200, BenefitName: "Noch offen"},
		},
	}}
	rec := newFakeClaimRecord("old")
	justClaimed := map[string]bool{"fresh": true} // from auto-claim in this cycle

	entries := recordObservedClaims(camps, rec, justClaimed)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one history entry (only 'fresh' is new), got %d", len(entries))
	}
	if !rec.IsDropClaimed("fresh") {
		t.Fatal("the freshly claimed tier must be in the record")
	}
	if rec.IsDropClaimed("open") {
		t.Fatal("an unclaimed drop must never be recorded")
	}

	// The names must be recorded too — once the campaign expires they're no
	// longer retrievable from Twitch, and the ID alone would be worthless.
	e := entries[0]
	if e.DropID != "fresh" || e.Reward != "30 Tournament Coins" ||
		e.Campaign != "MarbleFest - July'26-Day2" || e.Game != "Marbles on Stream" {
		t.Fatalf("history entry lost its names: %+v", e)
	}
	if e.Source != "auto" {
		t.Fatalf("drop claimed by us should be marked 'auto', got %q", e.Source)
	}
	if e.At.IsZero() {
		t.Fatal("history entry needs a timestamp")
	}

	// Second pass: nothing new, so no entry — otherwise the received-drops
	// history would list the same drops again on every cycle.
	if again := recordObservedClaims(camps, rec, justClaimed); len(again) != 0 {
		t.Fatalf("re-recording the same claims must produce no entries, got %d", len(again))
	}
}

// TestRecordObservedClaims_ExternalClaimMarked: a drop the user picked up on
// Twitch themselves, we only see as "is claimed" in the inventory. It still
// belongs in the received-drops history, but marked as "extern".
func TestRecordObservedClaims_ExternalClaimMarked(t *testing.T) {
	camps := []twitch.DropCampaign{{
		ID: "camp", Name: "Anno 117 - Summer Drops", GameName: "Anno 117: Pax Romana",
		Drops: []twitch.TimeBasedDrop{
			{ID: "manual", RequiredMinutesWatched: 60, IsClaimed: true, BenefitName: "Villa-Skin"},
		},
	}}
	rec := newFakeClaimRecord()

	entries := recordObservedClaims(camps, rec, nil) // nil = auto-claim got nothing
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	if entries[0].Source != "extern" {
		t.Fatalf("a drop we didn't claim ourselves should be 'extern', got %q", entries[0].Source)
	}
	if entries[0].Game != "Anno 117: Pax Romana" {
		t.Fatalf("game name lost: %+v", entries[0])
	}
}

// TestClaimHistory_AppendAndRead: append and read via a real file, including
// the two properties that matter — newest first, and a broken line doesn't
// make the whole list unreadable.
func TestClaimHistory_AppendAndRead(t *testing.T) {
	dir := t.TempDir()
	h := newClaimHistory(filepath.Join(dir, "config.json"))

	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	if err := h.Append([]ClaimHistoryEntry{
		{At: base, Game: "Marbles on Stream", Campaign: "Day1", Reward: "Alt", DropID: "a"},
		{At: base.Add(time.Hour), Game: "Anno 117: Pax Romana", Campaign: "Summer", Reward: "Neu", DropID: "b"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Smuggle in a broken line (e.g. an interrupted write).
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString("{this is not JSON\n")
	f.Close()

	got, err := h.Read(0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 readable entries despite the broken line, got %d", len(got))
	}
	if got[0].DropID != "b" {
		t.Fatalf("newest entry must come first, got %q", got[0].DropID)
	}

	// Limit is respected.
	if lim, _ := h.Read(1); len(lim) != 1 {
		t.Fatalf("limit ignored, got %d entries", len(lim))
	}
}

// TestClaimHistory_MissingFileIsNoError: before the first drop is received
// the file doesn't exist — the web UI should then see an empty list, not an error.
func TestClaimHistory_MissingFileIsNoError(t *testing.T) {
	h := newClaimHistory(filepath.Join(t.TempDir(), "config.json"))
	got, err := h.Read(0)
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d", len(got))
	}
}

// TestClaimedCounts_IgnoresNonWatchableDrops: sub-gated / reward-only tiers
// carry no watch-time requirement. Counting them would show "5/6 received" for
// a campaign that is actually done, which is the opposite of what the display
// is for.
func TestClaimedCounts_IgnoresNonWatchableDrops(t *testing.T) {
	c := twitch.DropCampaign{Drops: []twitch.TimeBasedDrop{
		{ID: "d1", RequiredMinutesWatched: 120, IsClaimed: true},
		{ID: "d2", RequiredMinutesWatched: 240, IsClaimed: true},
		{ID: "sub", RequiredMinutesWatched: 0}, // only reachable with a sub
	}}
	claimed, watchable := claimedCounts(c)
	if claimed != 2 || watchable != 2 {
		t.Fatalf("expected 2/2 (sub-gated tier ignored), got %d/%d", claimed, watchable)
	}
}

// TestClaimHistory_EnsureWritable: creates the file when it's missing, and
// reports an error when it's not writable. The latter is the case from
// 2026-07-28 (wrong owner + cap_drop ALL in the container).
func TestClaimHistory_EnsureWritable(t *testing.T) {
	dir := t.TempDir()
	h := newClaimHistory(filepath.Join(dir, "config.json"))

	if err := h.ensureWritable(); err != nil {
		t.Fatalf("file should be creatable: %v", err)
	}
	if _, err := os.Stat(h.path); err != nil {
		t.Fatalf("file was not created: %v", err)
	}

	// Revoke write permission → must be reported. File permissions don't
	// apply as root, so this is skipped (CI often runs as root).
	if os.Geteuid() == 0 {
		t.Skip("file permissions don't apply as root")
	}
	if err := os.Chmod(h.path, 0444); err != nil {
		t.Fatal(err)
	}
	if err := h.ensureWritable(); err == nil {
		t.Fatal("an unwritable file must report an error")
	}
}
