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
