package farmer

import "testing"

// Pure-function unit tests for the applySelectorPick guards. Full integration
// coverage of the Farmer state machine is deferred (would require a fake GQL
// client + lots of plumbing); these tests pin down the helpers that decide
// whether a pick is acceptable, which is where most of the recent regressions
// have been.

func TestPickGameMatches(t *testing.T) {
	tests := []struct {
		name        string
		campaigns   []CampaignRef
		currentGame string
		want        bool
	}{
		{
			name:        "exact match single campaign",
			campaigns:   []CampaignRef{{GameName: "Arena Breakout: Infinite"}},
			currentGame: "Arena Breakout: Infinite",
			want:        true,
		},
		{
			name:        "case-insensitive match",
			campaigns:   []CampaignRef{{GameName: "Escape From Tarkov"}},
			currentGame: "escape from tarkov",
			want:        true,
		},
		{
			name: "match against second campaign of multi-campaign pick",
			campaigns: []CampaignRef{
				{GameName: "Other Game"},
				{GameName: "Arena Breakout: Infinite"},
			},
			currentGame: "Arena Breakout: Infinite",
			want:        true,
		},
		{
			name:        "streamer switched to unrelated game — no match",
			campaigns:   []CampaignRef{{GameName: "Arena Breakout: Infinite"}},
			currentGame: "Just Chatting",
			want:        false,
		},
		{
			name:        "empty current game (offline) — no match",
			campaigns:   []CampaignRef{{GameName: "Arena Breakout: Infinite"}},
			currentGame: "",
			want:        false,
		},
		{
			name:        "no campaigns — no match",
			campaigns:   nil,
			currentGame: "Arena Breakout: Infinite",
			want:        false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pick := &PoolEntry{Campaigns: tc.campaigns}
			if got := pickGameMatches(pick, tc.currentGame); got != tc.want {
				t.Fatalf("pickGameMatches(%q) = %v, want %v", tc.currentGame, got, tc.want)
			}
		})
	}
}

func TestPickCampaignGames_DedupesCaseInsensitive(t *testing.T) {
	pick := &PoolEntry{Campaigns: []CampaignRef{
		{GameName: "Arena Breakout: Infinite"},
		{GameName: "arena breakout: infinite"},
		{GameName: "Escape from Tarkov"},
	}}
	got := pickCampaignGames(pick)
	// Order preserves insertion of the first-seen casing
	want := "Arena Breakout: Infinite,Escape from Tarkov"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
