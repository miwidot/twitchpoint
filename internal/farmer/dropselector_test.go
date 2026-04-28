package farmer

import (
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// fixed reference time for deterministic tests
var testNow = time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)

func newTestSelector(cfg *config.Config) *DropSelector {
	return &DropSelector{
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
	byGame map[string][]twitch.GameStream
	calls  map[string]int // game name → how often queried
}

func (f *fakeStreamSource) GetGameStreamsDropsEnabled(gameName string, limit int) ([]twitch.GameStream, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[gameName]++
	streams := f.byGame[gameName]
	if len(streams) > limit {
		return streams[:limit], nil
	}
	return streams, nil
}

func newSelectorWithStreams(cfg *config.Config, src *fakeStreamSource) *DropSelector {
	return &DropSelector{
		cfg:     cfg,
		streams: src,
		now:     func() time.Time { return testNow },
	}
}

func TestBuildPool_AllowListIntersection(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"Arena Breakout: Infinite": {
			{BroadcasterID: "1", BroadcasterLogin: "buggy", DisplayName: "Buggy", ViewerCount: 700},
			{BroadcasterID: "2", BroadcasterLogin: "kritikal", DisplayName: "kritikal", ViewerCount: 200},
			{BroadcasterID: "3", BroadcasterLogin: "randomdude", DisplayName: "RandomDude", ViewerCount: 100},
		},
	}}
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
		EndAt: testNow.Add(5 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: nil, // unrestricted
	}

	pool := sel.buildPool([]twitch.DropCampaign{camp})
	if len(pool) != 2 {
		t.Fatalf("unrestricted campaign should add all directory streams, got %d", len(pool))
	}
}

func TestBuildPool_DedupesAcrossCampaigns(t *testing.T) {
	cfg := &config.Config{}
	src := &fakeStreamSource{byGame: map[string][]twitch.GameStream{
		"ABI": {
			{BroadcasterID: "1", BroadcasterLogin: "buggy", ViewerCount: 700},
		},
	}}
	sel := newSelectorWithStreams(cfg, src)

	c1 := twitch.DropCampaign{
		ID: "abi-1", Name: "Partner-Only", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(4 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
		Channels: []twitch.DropChannel{{ID: "1", Name: "buggy"}},
	}
	c2 := twitch.DropCampaign{
		ID: "abi-2", Name: "Support Partners", Status: "ACTIVE", IsAccountConnected: true, GameName: "ABI",
		EndAt: testNow.Add(20 * 24 * time.Hour),
		Drops: []twitch.TimeBasedDrop{makeWatchableDrop()},
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
