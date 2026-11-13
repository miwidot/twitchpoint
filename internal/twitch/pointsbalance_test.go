package twitch

import (
	"encoding/json"
	"strings"
	"testing"
)

// decode turns a literal GQL response body into the map shape the real
// client hands to parsePointsBalance, so the tests exercise the same
// float64-typed numbers the JSON decoder actually produces.
func decode(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return data
}

// The shape Twitch returns today, captured from a live call on 2026-11-13.
func TestParsePointsBalance_LiveShape(t *testing.T) {
	data := decode(t, `{"channel":{"self":{"communityPoints":{"balance":159830}}}}`)
	got, err := parsePointsBalance(data)
	if err != nil {
		t.Fatalf("parsePointsBalance: %v", err)
	}
	if got != 159830 {
		t.Fatalf("balance = %d, want 159830", got)
	}
}

// A real zero must come back as a zero WITHOUT an error — the whole point
// of the fix is that those two cases stopped being indistinguishable.
func TestParsePointsBalance_GenuineZero(t *testing.T) {
	data := decode(t, `{"channel":{"self":{"communityPoints":{"balance":0}}}}`)
	got, err := parsePointsBalance(data)
	if err != nil {
		t.Fatalf("a zero balance must not be an error, got: %v", err)
	}
	if got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestParsePointsBalance_Failures(t *testing.T) {
	cases := []struct {
		name string
		body string
		// wantMsg is a fragment that has to appear, so each level of the
		// response keeps its own diagnosis instead of one generic error.
		wantMsg string
	}{
		{
			name:    "empty response",
			body:    `{}`,
			wantMsg: "no channel object",
		},
		{
			name:    "unknown login — channel is null",
			body:    `{"channel":null}`,
			wantMsg: "no channel object",
		},
		{
			name:    "no viewer context",
			body:    `{"channel":{"self":null}}`,
			wantMsg: "no viewer context",
		},
		{
			name:    "channel points switched off",
			body:    `{"channel":{"self":{"communityPoints":null}}}`,
			wantMsg: "no communityPoints",
		},
		{
			name:    "balance absent",
			body:    `{"channel":{"self":{"communityPoints":{}}}}`,
			wantMsg: "not a number",
		},
		{
			// The old pre-2026-11 shape, where balance was an object with
			// availablePoints. If Twitch ever serves it again this must
			// fail loudly rather than report zero points.
			name:    "legacy nested balance object",
			body:    `{"channel":{"self":{"communityPoints":{"balance":{"availablePoints":1234}}}}}`,
			wantMsg: "not a number",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePointsBalance(decode(t, c.body))
			if err == nil {
				t.Fatalf("expected an error, got balance %d", got)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, c.wantMsg)
			}
			if got != 0 {
				t.Errorf("balance = %d on error, want 0", got)
			}
		})
	}
}

// The query has to ask for the field the parser reads. These two drifted
// apart once already: the query kept asking for the removed `community`
// root while the parser had been taught the new shape, and the result was
// a continuous silent failure.
func TestQueryChannelPointsBalance_MatchesParser(t *testing.T) {
	if strings.Contains(queryChannelPointsBalance, "community(name:") {
		t.Error("query still uses the removed `community` root field")
	}
	for _, want := range []string{"channel(name: $channelLogin)", "self", "communityPoints", "balance"} {
		if !strings.Contains(queryChannelPointsBalance, want) {
			t.Errorf("query does not select %q", want)
		}
	}
	// availablePoints belonged to the old nested shape; selecting it again
	// would make balance an object and break the parser.
	if strings.Contains(queryChannelPointsBalance, "availablePoints") {
		t.Error("query selects availablePoints, which the parser no longer reads")
	}
}
