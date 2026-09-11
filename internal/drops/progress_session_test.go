package drops

import (
	"testing"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/twitch"
)

// The session named WARDOGS Beta & Launch's claimed 30-min drop while the
// pick was farming Early Access 0.1, whose drops need 660+ minutes.
func TestProgressFromSession_SiblingCampaignDropNeverCompletes(t *testing.T) {
	session := &twitch.CurrentDropSession{DropID: "wardog", CurrentMinutesWatched: 648, RequiredMinutesWatched: 30}

	p, done := progressFromSession("early-access", session, false)

	if done {
		t.Fatal("a sibling campaign's finished drop marked the picked campaign's drop done")
	}
	if p.CampaignID != "early-access" || p.CurrentMinutesWatched != 648 {
		t.Fatalf("minutes must still be applied to the pick, got %+v", p)
	}
	if p.DropID != "" || p.RequiredMinutesWatched != 0 {
		t.Fatalf("sibling drop ID/required leaked into the pick's progress: %+v", p)
	}
}

func TestProgressFromSession_OwnedDrop(t *testing.T) {
	cases := []struct {
		current, required int
		wantDone          bool
	}{
		{647, 660, false},
		{660, 660, true},
		{700, 660, true},
		{10, 0, false},
	}
	for _, c := range cases {
		session := &twitch.CurrentDropSession{DropID: "silver-2", CurrentMinutesWatched: c.current, RequiredMinutesWatched: c.required}
		p, done := progressFromSession("early-access", session, true)
		if done != c.wantDone {
			t.Fatalf("%d/%d: done=%v, want %v", c.current, c.required, done, c.wantDone)
		}
		if p.DropID != "silver-2" || p.RequiredMinutesWatched != c.required {
			t.Fatalf("%d/%d: owned drop not passed through: %+v", c.current, c.required, p)
		}
	}
}

func TestCampaignOwnsDrop(t *testing.T) {
	s := &Service{campaignCache: map[string]twitch.DropCampaign{
		"early-access": {ID: "early-access", Drops: []twitch.TimeBasedDrop{{ID: "silver-2"}, {ID: "gold-4"}}},
		"beta-launch":  {ID: "beta-launch", Drops: []twitch.TimeBasedDrop{{ID: "wardog"}}},
	}}

	if !s.campaignOwnsDrop("early-access", "gold-4") {
		t.Fatal("own drop not recognised")
	}
	if s.campaignOwnsDrop("early-access", "wardog") {
		t.Fatal("sibling campaign's drop attributed to the pick")
	}
	if s.campaignOwnsDrop("uncached", "silver-2") {
		t.Fatal("uncached campaign must not claim ownership")
	}
}

func TestApplyProgressUpdate_SiblingSessionKeepsRowRequired(t *testing.T) {
	s := &Service{
		cfg:          &config.Config{},
		log:          func(string, ...interface{}) {},
		writeLogFile: func(string) {},
		campaignCache: map[string]twitch.DropCampaign{
			"early-access": {ID: "early-access", Drops: []twitch.TimeBasedDrop{{ID: "silver-2", RequiredMinutesWatched: 660}}},
		},
		activeDrops: []ActiveDrop{{CampaignID: "early-access", DropName: "Silver Drop 2", Progress: 640, Required: 660}},
	}
	session := &twitch.CurrentDropSession{DropID: "wardog", CurrentMinutesWatched: 648, RequiredMinutesWatched: 30}

	p, _ := progressFromSession("early-access", session, s.campaignOwnsDrop("early-access", "wardog"))
	s.ApplyProgressUpdate(p)

	row := s.activeDrops[0]
	assertConsistent(t, row)
	if row.Required != 660 || row.Progress != 648 || row.DropName != "Silver Drop 2" {
		t.Fatalf("row corrupted by sibling session: %+v", row)
	}
}
