package twitch

import "testing"

func TestParseInventoryResponse_MissingDataIsAnError(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"no currentUser":   {},
		"null currentUser": {"currentUser": nil},
		"no inventory":     {"currentUser": map[string]interface{}{}},
		"null inventory":   {"currentUser": map[string]interface{}{"inventory": nil}},
	}
	for name, data := range cases {
		if _, _, err := parseInventoryResponse(data); err == nil {
			t.Fatalf("%s: parsed as an empty inventory instead of failing", name)
		}
	}
}

func TestParseInventoryResponse_NothingInProgress(t *testing.T) {
	data := map[string]interface{}{"currentUser": map[string]interface{}{
		"inventory": map[string]interface{}{"dropCampaignsInProgress": nil, "gameEventDrops": []interface{}{}},
	}}

	campaigns, _, err := parseInventoryResponse(data)

	if err != nil {
		t.Fatalf("a valid inventory with nothing in progress failed: %v", err)
	}
	if len(campaigns) != 0 {
		t.Fatalf("expected no campaigns, got %d", len(campaigns))
	}
}
