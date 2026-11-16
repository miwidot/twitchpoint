package drops

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// fixed reference time for deterministic tests
var testNow = time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)

func newTestSelector(cfg *config.Config) *Selector {
	return &Selector{
		cfg:     cfg,
		streams: nil,
		now:     func() time.Time { return testNow },
	}
}

func makeWatchableDrop() twitch.TimeBasedDrop {
	return twitch.TimeBasedDrop{
		ID:                     "drop-1",
		Name:                   "Test Drop",
		RequiredMinutesWatched: 60,
		IsClaimed:              false,
	}
}

func TestFilterEligibleCampaigns(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisabledCampaigns = []string{"camp-disabled"}
	cfg.CompletedCampaigns = []string{"camp-completed"}

	tests := []struct {
		name     string
		campaign twitch.DropCampaign
		want     bool // true = passes filter
	}{
		{
			name: "active connected with watchable drop passes",
			campaign: twitch.DropCampaign{
				ID: "camp-good", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: true,
		},
		{
			name: "expired campaign dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-expired", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(-1 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "non-ACTIVE status dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-expired-status", Status: "EXPIRED", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "account not connected dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-noacct", Status: "ACTIVE", IsAccountConnected: false,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "disabled by user dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-disabled", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "marked completed dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-completed", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
			},
			want: false,
		},
		{
			name: "sub-only drops dropped (RequiredMinutesWatched=0)",
			campaign: twitch.DropCampaign{
				ID: "camp-subonly", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{
					{ID: "d1", RequiredMinutesWatched: 0, IsClaimed: false},
				},
			},
			want: false,
		},
		{
			name: "all drops claimed dropped",
			campaign: twitch.DropCampaign{
				ID: "camp-allclaimed", Status: "ACTIVE", IsAccountConnected: true,
				EndAt: testNow.Add(2 * time.Hour),
				Drops: []twitch.TimeBasedDrop{
					{ID: "d1", RequiredMinutesWatched: 60, IsClaimed: true},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel := newTestSelector(cfg)
			out := sel.filterEligibleCampaigns([]twitch.DropCampaign{tt.campaign})
			got := len(out) == 1
			if got != tt.want {
				t.Fatalf("got %d eligible (%v), want %v", len(out), got, tt.want)
			}
		})
	}
}

// fakeStreamSource is a deterministic in-memory stream source for tests.
type fakeStreamSource struct {
	byGame  map[string][]twitch.GameStream
	calls   map[string]int                 // fixture key (display name) → how often queried
	byLogin map[string]*twitch.ChannelInfo // login → ChannelInfo for ACL lookups
	// errBySlug makes a directory lookup fail for one slug, so tests can
	// tell "the lookup broke" apart from "nobody is streaming".
	errBySlug map[string]error
}

// GetGameStreamsDropsEnabled receives the game's URL slug — since the v2.0
// persisted-hash GameDirectory switch, production queries the directory by
// slug, not display name. Fixtures stay keyed by display name for
// readability; the fake resolves the slug back to the fixture key (and
// counts the call under it) so assertions can keep using display names.
func (f *fakeStreamSource) GetGameStreamsDropsEnabled(gameSlug string, limit int) ([]twitch.GameStream, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	if err, ok := f.errBySlug[gameSlug]; ok {
		f.calls[gameSlug]++
		return nil, err
	}
	key := gameSlug
	streams, ok := f.byGame[key]
	if !ok {
		for name, s := range f.byGame {
			if twitch.SlugFromGameName(name) == gameSlug {
				key, streams = name, s
				break
			}
		}
	}
	f.calls[key]++
	if len(streams) > limit {
		return streams[:limit], nil
	}
	return streams, nil
}

func (f *fakeStreamSource) GetChannelInfos(logins []string) []*twitch.ChannelInfo {
	out := make([]*twitch.ChannelInfo, len(logins))
	for i, l := range logins {
		out[i] = f.byLogin[l]
	}
	return out
}

func newSelectorWithStreams(cfg *config.Config, src *fakeStreamSource) *Selector {
	return &Selector{
		cfg:     cfg,
		streams: src,
		now:     func() time.Time { return testNow },
	}
}

func TestBuildPool_AllowListIntersection(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{
		byGame: map[string][]twitch.GameStream{
			"Arena Breakout: Infinite": {
				{BroadcasterID: "1", BroadcasterLogin: "buggy", DisplayName: "Buggy", ViewerCount: 700},
				{BroadcasterID: "2", BroadcasterLogin: "kritikal", DisplayName: "kritikal", ViewerCount: 200},
				{BroadcasterID: "3", BroadcasterLogin: "randomdude", DisplayName: "RandomDude", ViewerCount: 100},
			},
		},
		// v2.0: ACL campaigns now resolve allowed_channels directly via
		// GetChannelInfos, so the test must populate live ChannelInfos.
		byLogin: map[string]*twitch.ChannelInfo{
			"buggy":            {ID: "1", Login: "buggy", DisplayName: "Buggy", IsLive: true, GameName: "Arena Breakout: Infinite", ViewerCount: 700},
			"kritikal":         {ID: "2", Login: "kritikal", DisplayName: "kritikal", IsLive: true, GameName: "Arena Breakout: Infinite", ViewerCount: 200},
			"offline_streamer": {ID: "999", Login: "offline_streamer", DisplayName: "Offline", IsLive: false},
		},
	}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "abi-partner", Name: "ABI Partner-Only Drops",
		Status: "ACTIVE", IsAccountConnected: true, GameName: "Arena Breakout: Infinite",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{
			{ID: "1", Name: "buggy"},
			{ID: "2", Name: "kritikal"},
			{ID: "999", Name: "offline_streamer"},
		},
	}

	pool := sel.buildPool([]twitch.DropCampaign{camp})
	if len(pool) != 2 {
		t.Fatalf("want 2 pool entries (buggy + kritikal), got %d", len(pool))
	}
	logins := map[string]bool{}
	for _, e := range pool {
		logins[e.ChannelLogin] = true
	}
	if !logins["buggy"] || !logins["kritikal"] {
		t.Fatalf("expected buggy + kritikal, got %v", logins)
	}
	if logins["randomdude"] {
		t.Fatal("randomdude should be filtered (not in allow list)")
	}
}

func TestBuildPool_UnrestrictedCampaign(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"Marvel Rivals": {
			{BroadcasterID: "10", BroadcasterLogin: "streamer_a", ViewerCount: 5000},
			{BroadcasterID: "11", BroadcasterLogin: "streamer_b", ViewerCount: 3000},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "rivals-s7", Status: "ACTIVE", IsAccountConnected: true, GameName: "Marvel Rivals",
		EndAt:    testNow.Add(5 * time.Hour),
		Drops:    []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: nil, // unrestricted
	}

	pool := sel.buildPool([]twitch.DropCampaign{camp})
	if len(pool) != 2 {
		t.Fatalf("unrestricted campaign should add all directory streams, got %d", len(pool))
	}
}

func TestBuildPool_DedupesAcrossCampaigns(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{
		byGame: map[string][]twitch.GameStream{
			"ABI": {{BroadcasterID: "1", BroadcasterLogin: "buggy", ViewerCount: 700}},
		},
		byLogin: map[string]*twitch.ChannelInfo{
			"buggy": {ID: "1", Login: "buggy", DisplayName: "buggy", IsLive: true, GameName: "ABI", ViewerCount: 700},
		},
	}
	sel := newSelectorWithStreams(cfg, src)

	c1 := twitch.DropCampaign{
		ID: "abi-1", Name: "Partner-Only", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt:    testNow.Add(4 * time.Hour),
		Drops:    []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{{ID: "1", Name: "buggy"}},
	}
	c2 := twitch.DropCampaign{
		ID: "abi-2", Name: "Support Partners", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt:    testNow.Add(20 * 24 * time.Hour),
		Drops:    []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{{ID: "1", Name: "buggy"}},
	}

	pool := sel.buildPool([]twitch.DropCampaign{c1, c2})
	if len(pool) != 1 {
		t.Fatalf("buggy in 2 campaigns should yield 1 deduped pool entry, got %d", len(pool))
	}
	if len(pool[0].Campaigns) != 2 {
		t.Fatalf("deduped entry should carry both campaigns, got %d", len(pool[0].Campaigns))
	}
}

func TestBuildPool_DirectoryQueriedOncePerGame(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {{BroadcasterID: "1", BroadcasterLogin: "buggy"}},
	}}
	sel := newSelectorWithStreams(cfg, src)

	c1 := twitch.DropCampaign{
		ID: "abi-1", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}
	c2 := twitch.DropCampaign{
		ID: "abi-2", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(5 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	sel.buildPool([]twitch.DropCampaign{c1, c2})
	if got := src.calls["ABI"]; got != 1 {
		t.Fatalf("directory should be queried once per cycle per game, got %d calls", got)
	}
}

func TestSortPool_WantedGamesPriority(t *testing.T) {
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"Game A", "Game B"} // A=rank 0, B=rank 1
	sel := newTestSelector(cfg)

	a := &PoolEntry{ChannelLogin: "for-a", ViewerCount: 100, Campaigns: []CampaignRef{
		{GameName: "Game A", EndAt: testNow.Add(20 * time.Hour)},
	}}
	b := &PoolEntry{ChannelLogin: "for-b", ViewerCount: 1000, Campaigns: []CampaignRef{
		{GameName: "Game B", EndAt: testNow.Add(2 * time.Hour)}, // closer expiry but lower-priority game
	}}

	pool := []*PoolEntry{b, a}
	sel.sortPool(pool)

	if pool[0].ChannelLogin != "for-a" {
		t.Fatalf("Game A channel should win regardless of viewers/expiry, got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_NotInWantedSortsLast(t *testing.T) {
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"Wanted Game"}
	sel := newTestSelector(cfg)

	wanted := &PoolEntry{ChannelLogin: "wanted", Campaigns: []CampaignRef{
		{GameName: "Wanted Game", EndAt: testNow.Add(20 * time.Hour)},
	}}
	other := &PoolEntry{ChannelLogin: "other", Campaigns: []CampaignRef{
		{GameName: "Some Other Game", EndAt: testNow.Add(2 * time.Hour)},
	}}

	pool := []*PoolEntry{other, wanted}
	sel.sortPool(pool)

	if pool[0].ChannelLogin != "wanted" {
		t.Fatalf("wanted-game channel should sort first, got %s", pool[0].ChannelLogin)
	}
	if pool[1].ChannelLogin != "other" {
		t.Fatalf("non-wanted should sort after wanted, got %s", pool[1].ChannelLogin)
	}
}

func TestSortPool_MultiCampaignChannelUsesBestGameRank(t *testing.T) {
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"High", "Low"}
	sel := newTestSelector(cfg)

	multi := &PoolEntry{ChannelLogin: "multi", Campaigns: []CampaignRef{
		{GameName: "Low", EndAt: testNow.Add(5 * time.Hour)},
		{GameName: "High", EndAt: testNow.Add(20 * time.Hour)}, // best game-rank for this channel
	}}
	lowOnly := &PoolEntry{ChannelLogin: "lowOnly", Campaigns: []CampaignRef{
		{GameName: "Low", EndAt: testNow.Add(2 * time.Hour)},
	}}

	pool := []*PoolEntry{lowOnly, multi}
	sel.sortPool(pool)

	if pool[0].ChannelLogin != "multi" {
		t.Fatalf("multi-campaign channel should win because it covers High game, got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_EarlierEndAtFirst(t *testing.T) {
	cfg := &config.Config{}
	sel := newTestSelector(cfg)

	near := &PoolEntry{ChannelLogin: "near", Campaigns: []CampaignRef{
		{EndAt: testNow.Add(2 * time.Hour)},
	}}
	far := &PoolEntry{ChannelLogin: "far", Campaigns: []CampaignRef{
		{EndAt: testNow.Add(20 * time.Hour)},
	}}

	pool := []*PoolEntry{far, near}
	sel.sortPool(pool)
	if pool[0].ChannelLogin != "near" {
		t.Fatalf("near-expiry channel should sort first, got %s", pool[0].ChannelLogin)
	}
}

func TestSortPool_ViewerCountTieBreak(t *testing.T) {
	cfg := &config.Config{}
	sel := newTestSelector(cfg)

	endAt := testNow.Add(2 * time.Hour)
	low := &PoolEntry{ChannelLogin: "low", ViewerCount: 100, Campaigns: []CampaignRef{{EndAt: endAt}}}
	high := &PoolEntry{ChannelLogin: "high", ViewerCount: 1000, Campaigns: []CampaignRef{{EndAt: endAt}}}

	pool := []*PoolEntry{low, high}
	sel.sortPool(pool)
	if pool[0].ChannelLogin != "high" {
		t.Fatalf("higher viewer count should win tie, got %s", pool[0].ChannelLogin)
	}
}

func TestSelect_EmptyPoolReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {}, // game returns no live streams
	}}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "abi", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	pick, queue := sel.Select([]twitch.DropCampaign{camp}, nil)
	if pick != nil || queue != nil {
		t.Fatalf("empty stream directory should yield nil pick, got %v / %v", pick, queue)
	}
}

func TestSelect_WantedGamesForcesNonClosestExpiry(t *testing.T) {
	// v1.8.0 equivalent of the old pin test: wanted_games priority overrides remaining_time.
	cfg := &config.Config{}
	cfg.GamesToWatch = []string{"GameB"} // only GameB is wanted
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"GameA": {{BroadcasterID: "1", BroadcasterLogin: "near_streamer"}},
		"GameB": {{BroadcasterID: "2", BroadcasterLogin: "far_streamer"}},
	}}
	sel := newSelectorWithStreams(cfg, src)

	near := twitch.DropCampaign{
		ID: "near", Status: "ACTIVE", IsAccountConnected: true, GameName: "GameA",
		EndAt: testNow.Add(2 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}
	wantedFar := twitch.DropCampaign{
		ID: "wanted-far", Status: "ACTIVE", IsAccountConnected: true, GameName: "GameB",
		EndAt: testNow.Add(20 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	pick, _ := sel.Select([]twitch.DropCampaign{near, wantedFar}, nil)
	if pick == nil || pick.ChannelLogin != "far_streamer" {
		t.Fatalf("wanted_games should override closest-expiry sort, picked %v", pick)
	}
}

func TestSelect_SkipChannelsExcludesFromPool(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {
			{BroadcasterID: "1", BroadcasterLogin: "stalled_streamer", ViewerCount: 700},
			{BroadcasterID: "2", BroadcasterLogin: "healthy_streamer", ViewerCount: 200},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	camp := twitch.DropCampaign{
		ID: "abi", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
	}

	// No skip → top viewer "stalled_streamer" wins
	pick, _ := sel.Select([]twitch.DropCampaign{camp}, nil)
	if pick == nil || pick.ChannelLogin != "stalled_streamer" {
		t.Fatalf("without skip, top viewer should win, got %v", pick)
	}

	// Skip "stalled_streamer" → fallback to "healthy_streamer"
	skip := map[string]bool{"1": true}
	pick, _ = sel.Select([]twitch.DropCampaign{camp}, skip)
	if pick == nil || pick.ChannelLogin != "healthy_streamer" {
		t.Fatalf("with skip, fallback should be healthy_streamer, got %v", pick)
	}

	// Skip both → empty pool
	skipBoth := map[string]bool{"1": true, "2": true}
	pick, _ = sel.Select([]twitch.DropCampaign{camp}, skipBoth)
	if pick != nil {
		t.Fatalf("skipping every channel should yield nil pick, got %v", pick)
	}
}

// --- directory diagnostics -------------------------------------------
//
// An empty pool used to be unreadable: a failed directory lookup, a game
// nobody is streaming and a campaign that restricts its channels all
// produced poolSize=0 and nothing else. These tests pin the difference,
// because the first case is the one that silently costs drops.

func unrestrictedCampaign(game string) twitch.DropCampaign {
	return twitch.DropCampaign{
		ID: "c-" + game, Status: "ACTIVE", IsAccountConnected: true, GameName: game,
		EndAt:    testNow.Add(5 * time.Hour),
		Drops:    []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: nil,
	}
}

func TestSelect_DirectoryFailureIsReportedNotSwallowed(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{errBySlug: map[string]error{
		"escape-from-tarkov": errors.New("gql error: service unavailable"),
	}}
	sel := newSelectorWithStreams(cfg, src)

	var diags []string
	sel.SetDiagSink(func(format string, args ...interface{}) {
		diags = append(diags, fmt.Sprintf(format, args...))
	})

	pick, _ := sel.Select([]twitch.DropCampaign{unrestrictedCampaign("Escape from Tarkov")}, nil)
	if pick != nil {
		t.Fatal("a failed directory lookup must not produce a pick")
	}
	summary := sel.LastDirSummary()
	if !strings.Contains(summary, "escape-from-tarkov=ERROR") {
		t.Errorf("summary must name the failing lookup, got %q", summary)
	}
	if !strings.Contains(summary, "service unavailable") {
		t.Errorf("summary must carry the underlying error, got %q", summary)
	}
	joined := strings.Join(diags, "\n")
	if !strings.Contains(joined, "directory lookup failed") {
		t.Errorf("the failure must reach the diag sink, got: %s", joined)
	}
}

func TestSelect_EmptyDirectoryIsNotAnError(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{"Escape from Tarkov": {}}}
	sel := newSelectorWithStreams(cfg, src)

	pick, _ := sel.Select([]twitch.DropCampaign{unrestrictedCampaign("Escape from Tarkov")}, nil)
	if pick != nil {
		t.Fatal("no live streams means no pick")
	}
	if got, want := sel.LastDirSummary(), "escape-from-tarkov=0"; got != want {
		t.Errorf("summary = %q, want %q — a quiet directory must read as 0, not as an error", got, want)
	}
}

func TestSelect_DirectorySummaryCountsLiveStreams(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"Escape from Tarkov": {
			{BroadcasterID: "1", BroadcasterLogin: "a", ViewerCount: 10},
			{BroadcasterID: "2", BroadcasterLogin: "b", ViewerCount: 5},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	pick, pool := sel.Select([]twitch.DropCampaign{unrestrictedCampaign("Escape from Tarkov")}, nil)
	if pick == nil || len(pool) != 2 {
		t.Fatalf("expected a pick from 2 candidates, got pick=%v pool=%d", pick, len(pool))
	}
	if got, want := sel.LastDirSummary(), "escape-from-tarkov=2"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// With nothing eligible the pool is never built, so the previous cycle's
// numbers must not be reported against this one. They were: the log showed
// a stale poolSize=3 for cycles that built no pool at all.
func TestSelect_StaleStatsClearedWhenNothingEligible(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"Escape from Tarkov": {
			{BroadcasterID: "1", BroadcasterLogin: "a", ViewerCount: 10},
			{BroadcasterID: "2", BroadcasterLogin: "b", ViewerCount: 5},
			{BroadcasterID: "3", BroadcasterLogin: "c", ViewerCount: 1},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	if _, pool := sel.Select([]twitch.DropCampaign{unrestrictedCampaign("Escape from Tarkov")}, nil); len(pool) != 3 {
		t.Fatalf("setup: expected 3 candidates, got %d", len(pool))
	}
	if sel.LastPoolSize() != 3 {
		t.Fatalf("setup: LastPoolSize = %d, want 3", sel.LastPoolSize())
	}

	// Second cycle: the campaign has expired, so nothing is eligible.
	expired := unrestrictedCampaign("Escape from Tarkov")
	expired.EndAt = testNow.Add(-time.Hour)
	sel.Select([]twitch.DropCampaign{expired}, nil)

	if got := sel.LastPoolSize(); got != 0 {
		t.Errorf("LastPoolSize = %d after a cycle that built no pool, want 0", got)
	}
	if got := sel.LastDirSummary(); got != "-" {
		t.Errorf("LastDirSummary = %q after a cycle that queried no directory, want %q", got, "-")
	}
}
