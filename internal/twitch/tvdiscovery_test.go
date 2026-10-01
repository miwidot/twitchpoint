package twitch

import "testing"

func campaignJSON(summaryMW bool, drops []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id":    "camp-1",
		"name":  "Test Campaign",
		"endAt": "2026-10-22T16:00:00Z",
		"summary": map[string]interface{}{
			"includesMWRequirement": summaryMW,
		},
		"timeBasedDrops": drops,
	}
}

func watchDrop(id string, required float64) map[string]interface{} {
	return map[string]interface{}{
		"id":                     id,
		"name":                   "Drop " + id,
		"startAt":                "2026-09-25T16:00:00.671Z",
		"endAt":                  "2026-10-22T16:00:00Z",
		"requiredMinutesWatched": required,
		"benefitEdges": []interface{}{
			map[string]interface{}{
				"benefit": map[string]interface{}{
					"id":               "ben-" + id,
					"name":             "Reward " + id,
					"distributionType": "DIRECT_ENTITLEMENT",
				},
			},
		},
	}
}

// A sub-reward campaign awards on subscriptions, not watch time — farming it
// is impossible, so it must never reach the selector. This is the exact shape
// the live query returned for SUBATHON2026 (requiredSubs set, 0 minutes).
func TestParseAvailableDropCampaign_SkipsSubRewardCampaign(t *testing.T) {
	subDrop := map[string]interface{}{
		"id":                     "d1",
		"requiredMinutesWatched": float64(0),
		"requiredSubs":           float64(5),
	}
	if c := parseAvailableDropCampaign(campaignJSON(false, []interface{}{subDrop})); c != nil {
		t.Fatalf("sub-reward campaign must be skipped, got %+v", c)
	}
}

// Even if the summary claims a watch requirement, a campaign whose drops all
// need zero minutes has nothing to farm.
func TestParseAvailableDropCampaign_SkipsCampaignWithoutWatchableDrops(t *testing.T) {
	zero := map[string]interface{}{"id": "d1", "requiredMinutesWatched": float64(0)}
	if c := parseAvailableDropCampaign(campaignJSON(true, []interface{}{zero})); c != nil {
		t.Fatalf("campaign without watchable drops must be skipped, got %+v", c)
	}
}

func TestParseAvailableDropCampaign_ParsesWatchCampaign(t *testing.T) {
	m := campaignJSON(true, []interface{}{watchDrop("d1", 60), watchDrop("d2", 120)})

	c := parseAvailableDropCampaign(m)

	if c == nil {
		t.Fatal("watch-based campaign was skipped")
	}
	if c.ID != "camp-1" || c.Name != "Test Campaign" {
		t.Fatalf("campaign fields wrong: %+v", c)
	}
	if len(c.Drops) != 2 {
		t.Fatalf("got %d drops, want 2", len(c.Drops))
	}
	if c.Drops[0].RequiredMinutesWatched != 60 || c.Drops[1].RequiredMinutesWatched != 120 {
		t.Fatalf("required minutes not parsed: %+v", c.Drops)
	}
	if c.Drops[0].BenefitID != "ben-d1" || c.Drops[0].BenefitName != "Reward d1" {
		t.Fatalf("benefit not parsed: %+v", c.Drops[0])
	}
	if c.EndAt.IsZero() {
		t.Fatal("campaign endAt not parsed")
	}
	// The query carries no per-user state; the inventory merge supplies it.
	// Staying "connected" keeps the campaign visible until then.
	if !c.IsAccountConnected {
		t.Fatal("campaign should default to connected until inventory says otherwise")
	}
}

// Drops below the campaign level that need no watch time are dropped
// individually, while the watchable ones survive.
func TestParseAvailableDropCampaign_DropsZeroMinuteDropsOnly(t *testing.T) {
	zero := map[string]interface{}{"id": "sub", "requiredMinutesWatched": float64(0)}
	m := campaignJSON(true, []interface{}{zero, watchDrop("d2", 90)})

	c := parseAvailableDropCampaign(m)

	if c == nil {
		t.Fatal("campaign with one watchable drop must survive")
	}
	if len(c.Drops) != 1 || c.Drops[0].ID != "d2" {
		t.Fatalf("expected only the watchable drop, got %+v", c.Drops)
	}
}

func TestParseAvailableDropCampaign_RejectsMissingID(t *testing.T) {
	m := campaignJSON(true, []interface{}{watchDrop("d1", 60)})
	delete(m, "id")
	if c := parseAvailableDropCampaign(m); c != nil {
		t.Fatal("campaign without an ID must be rejected")
	}
}

// Discovery iterates wanted games; with none configured there is nothing to
// search, and silently returning an empty catalogue would look like "no
// campaigns exist" rather than a misconfiguration.
func TestDiscoverCampaignsByGame_NoWantedGamesIsAnError(t *testing.T) {
	g := &GQLClient{}
	if _, err := g.discoverCampaignsByGame(nil); err == nil {
		t.Fatal("expected an error when no wanted games are configured")
	}
}

func TestAppendChannelOnce_Deduplicates(t *testing.T) {
	list := appendChannelOnce(nil, DropChannel{ID: "1", Name: "a"})
	list = appendChannelOnce(list, DropChannel{ID: "1", Name: "a"})
	list = appendChannelOnce(list, DropChannel{ID: "2", Name: "b"})
	if len(list) != 2 {
		t.Fatalf("expected 2 unique channels, got %d: %+v", len(list), list)
	}
}

// The identity must travel as a unit: a Client-Id paired with another
// client's User-Agent is accepted by Twitch but earns no watch time.
func TestActiveClient_SwitchesIdentityAsAUnit(t *testing.T) {
	t.Cleanup(func() { SetActiveClient(ClientAndroid) })

	SetActiveClient(ClientAndroid)
	if IsTVClient() {
		t.Fatal("android client must not report as TV")
	}
	if ActiveClient().ClientID != ClientAndroid.ClientID || ActiveClient().UserAgent != ClientAndroid.UserAgent {
		t.Fatal("android identity not applied as a unit")
	}

	SetActiveClient(ClientTV)
	if !IsTVClient() {
		t.Fatal("TV client not detected")
	}
	if ActiveClient().ClientID != ClientTV.ClientID || ActiveClient().UserAgent != ClientTV.UserAgent {
		t.Fatal("TV identity not applied as a unit")
	}
}
