package twitch

import "sync/atomic"

// ClientType bundles the identity we present to Twitch: the Client-Id header
// and the matching User-Agent. The two MUST travel together — Twitch
// correlates them, and a token minted by one client used with another
// client's header is accepted but earns no watch time. Several miners
// learned that the hard way in September 2026.
type ClientType struct {
	Name      string
	ClientID  string
	UserAgent string
}

// ClientAndroid is the Twitch Android app. It bypasses the integrity-token
// requirement and is the only client whose tokens see the full campaign
// catalogue through ViewerDropsDashboard — which is why it stays the
// default. Twitch disabled its device-code login in September 2026, so new
// tokens can no longer be minted this way; existing ones keep working.
var ClientAndroid = ClientType{
	Name:      "android",
	ClientID:  "kd1unb4b3q4t58fwlpcbzcbnm76a8fp",
	UserAgent: "Dalvik/2.1.0 (Linux; U; Android 16; SM-S911B Build/TP1A.220624.014) tv.twitch.android.app/25.3.0/2503006",
}

// ClientTV is the Twitch Smart-TV/console client. Its device-code login
// still works, which makes it the only way to obtain a fresh token after
// the September 2026 change — but tokens it mints get `dropCampaigns: null`
// from ViewerDropsDashboard, so campaign discovery has to go the per-game
// route instead (see discoverCampaignsByGame).
var ClientTV = ClientType{
	Name:      "tv",
	ClientID:  "ue6666qo983tsx6so1t0vnawi233wa",
	UserAgent: "Mozilla/5.0 (Linux; Android 7.1; Smart Box C1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
}

// activeClient is read on every outgoing request, so it is swapped
// atomically. SetActiveClient runs once at startup, before any request.
var activeClient atomic.Pointer[ClientType]

func init() { activeClient.Store(&ClientAndroid) }

// SetActiveClient selects the identity used for all Twitch traffic.
func SetActiveClient(c ClientType) { activeClient.Store(&c) }

// ActiveClient returns the identity currently in use.
func ActiveClient() ClientType { return *activeClient.Load() }

// IsTVClient reports whether the TV client is active. Campaign discovery
// branches on this because ViewerDropsDashboard is useless for TV tokens.
func IsTVClient() bool { return activeClient.Load().Name == ClientTV.Name }
