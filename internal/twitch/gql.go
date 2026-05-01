package twitch

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	gqlURL     = "https://gql.twitch.tv/gql"
	TVClientID = "kd1unb4b3q4t58fwlpcbzcbnm76a8fp" // Android App client-ID — bypasses integrity tokens, supports ViewerDropsDashboard
	maxGQLBody = 8 * 1024 * 1024
)

// Raw GQL queries for read operations
const (
	queryGetUserInfo = `query { currentUser { id login displayName } }`

	queryGetChannelInfo = `query GetChannelInfo($login: String!) {
		user(login: $login) {
			id login displayName
			stream { id viewersCount game { id displayName } }
		}
	}`

	queryGetGameStreams = `query DirectoryPage_Game($name: String!, $first: Int!) {
		game(name: $name) {
			streams(first: $first) {
				edges {
					node {
						id
						broadcaster { id login displayName }
						viewersCount
						game { id name }
					}
				}
			}
		}
	}`

	// Same as queryGetGameStreams but filters for streams with the Drops Enabled
	// system filter — used when auto-selecting a channel for an unrestricted drop
	// campaign so we don't pick a streamer who isn't running drops.
	queryGetGameStreamsDropsEnabled = `query DirectoryPage_Game($name: String!, $first: Int!) {
		game(name: $name) {
			streams(first: $first, options: { sort: VIEWER_COUNT, systemFilters: [DROPS_ENABLED] }) {
				edges {
					node {
						id
						broadcaster { id login displayName }
						viewersCount
						game { id name }
					}
				}
			}
		}
	}`

	queryGetChannelNameByID = `query GetChannelNameByID($id: ID!) {
		user(id: $id) { displayName }
	}`

	queryGetChannelInfoByID = `query GetChannelInfoByID($id: ID!) {
		user(id: $id) {
			id login displayName
			stream { id viewersCount game { id displayName } }
		}
	}`

	queryChannelPointsBalance = `query ChannelPointsContext($channelLogin: String!) {
		community(name: $channelLogin) {
			channel {
				self { communityPoints { balance { availablePoints } } }
			}
		}
	}`
)

// Raw GQL mutations — TV Client-ID allows raw mutations without integrity token
const (
	mutationClaimCommunityPoints = `mutation ClaimCommunityPoints($input: ClaimCommunityPointsInput!) {
		claimCommunityPoints(input: $input) {
			claim { id }
			currentPoints
			error { code }
		}
	}`

	mutationJoinRaid = `mutation JoinRaid($input: JoinRaidInput!) {
		joinRaid(input: $input) {
			__typename
		}
	}`

	// Persisted query hash for JoinRaid (used as fallback if raw mutation fails)
	joinRaidHash = "c6a332a86d1087fbbb1a8623aa01bd1313d2386e7c63be60fdb2d1901f01a4ae"

	// Persisted query hash for DropCurrentSessionContext — returns the
	// (dropID, currentMinutesWatched) pair for the channel currently being
	// watched. Used as the v1.8.0 polling fallback because user-drop-events
	// PubSub doesn't always fire reliably (TDM has the same issue and uses
	// this query as the bridge after each Spade heartbeat).
	dropCurrentSessionHash = "4d06b702d25d652afb9ef835d2a550031f1cf762b193523a92166f40ea3d142b"
)

// CurrentDropSession is the lean response from DropCurrentSessionContext.
// Either field may be empty if no drop session is active for the channel.
type CurrentDropSession struct {
	DropID                 string
	CurrentMinutesWatched  int
	RequiredMinutesWatched int
}

// SendMinuteWatched sends a minute-watched event via the sendSpadeEvents GQL
// mutation. This is the modern (and only working) way to credit drop minutes
// — POST to the legacy spade.twitch.tv/track endpoint silently fails on
// stricter campaigns (ABI Partner-Only, etc.). DevilXD's TwitchDropsMiner
// uses this same path; see channel.py:_gql_payload + send_watch.
func (g *GQLClient) SendMinuteWatched(channelID, channelLogin, broadcastID, gameName, gameID, userID string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	// CRITICAL: user_id must be sent as INT, not string. TDM payload (verified
	// against the live container running ABI Partner-Only Drops on the same
	// account) sends `"user_id": 86551629` not `"user_id": "86551629"`. Twitch's
	// drop-credit pipeline validates the type — string user_id returns 204
	// (request accepted) but the credit is silently dropped.
	uidInt, _ := strconv.ParseInt(userID, 10, 64)
	payload := []map[string]interface{}{
		{
			"event": "minute-watched",
			"properties": map[string]interface{}{
				"broadcast_id":   broadcastID,
				"channel_id":     channelID,
				"channel":        channelLogin,
				"client_time":    now,
				"game":           gameName,
				"game_id":        gameID,
				"hidden":         false,
				"is_live":        true,
				"live":           true,
				"logged_in":      true,
				"minutes_logged": 1,
				"muted":          false,
				"user_id":        uidInt,
			},
		},
	}
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal spade payload: %w", err)
	}
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(jsonBytes); err != nil {
		return fmt.Errorf("gzip spade payload: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(gz.Bytes())

	req := &GQLRequest{
		OperationName: "SendEvents",
		Query: "mutation SendEvents($input: SendSpadeEventsInput!) {" +
			" sendSpadeEvents(input: $input) { statusCode }" +
			" }",
		Variables: map[string]interface{}{
			"input": map[string]interface{}{
				"data":       encoded,
				"repository": "twilight",
				"encoding":   "GZIP_B64",
			},
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return fmt.Errorf("send minute watched: %w", err)
	}
	sse, ok := resp.Data["sendSpadeEvents"].(map[string]interface{})
	if !ok || sse == nil {
		return fmt.Errorf("no sendSpadeEvents in response")
	}
	status := getInt(sse, "statusCode")
	if status != 204 {
		return fmt.Errorf("sendSpadeEvents returned statusCode %d (need 204)", status)
	}
	return nil
}

const queryPlaybackAccessToken = `query PlaybackAccessToken_Template($login: String!, $isLive: Boolean!, $vodID: ID!, $isVod: Boolean!, $playerType: String!) {
	streamPlaybackAccessToken(channelName: $login, params: {platform: "web", playerBackend: "mediaplayer", playerType: $playerType}) @include(if: $isLive) {
		value
		signature
	}
	videoPlaybackAccessToken(id: $vodID, params: {platform: "web", playerBackend: "mediaplayer", playerType: $playerType}) @include(if: $isVod) {
		value
		signature
	}
}`

// GetPlaybackAccessToken returns the HLS sig+token for a live channel. Used by
// StreamProber to fetch the m3u8 playlist so Twitch counts us as a real viewer
// for drop-credit purposes (Spade heartbeats alone are silently rejected by
// some campaigns — notably ABI Partner-Only and other anti-cheat-flagged ones).
func (g *GQLClient) GetPlaybackAccessToken(login string) (value, signature string, err error) {
	req := &GQLRequest{
		OperationName: "PlaybackAccessToken_Template",
		Query:         queryPlaybackAccessToken,
		Variables: map[string]interface{}{
			"login":      strings.ToLower(login),
			"isLive":     true,
			"vodID":      "",
			"isVod":      false,
			"playerType": "site",
		},
	}
	resp, err := g.do(req)
	if err != nil {
		return "", "", fmt.Errorf("playback access token: %w", err)
	}
	spat, ok := resp.Data["streamPlaybackAccessToken"].(map[string]interface{})
	if !ok || spat == nil {
		return "", "", fmt.Errorf("no streamPlaybackAccessToken in response")
	}
	value = getString(spat, "value")
	signature = getString(spat, "signature")
	if value == "" || signature == "" {
		return "", "", fmt.Errorf("empty token or signature")
	}
	return value, signature, nil
}

// GQLClient handles all Twitch GQL API calls.
type GQLClient struct {
	authToken       string
	httpClient      *http.Client
	deviceID        string // X-Device-Id header (32 alphanumeric, persisted per session)
	clientSessionID string // Client-Session-Id header (16 hex bytes, per session)
}

// NewGQLClient creates a new GQL client with the given auth token.
// Tries to fetch a real unique_id from Twitch's Set-Cookie header at startup
// (matches TDM auth_state.py behavior). If the fetch fails, falls back to a
// locally-generated random id.
func NewGQLClient(authToken string) *GQLClient {
	deviceID := fetchTwitchUniqueID()
	if deviceID == "" {
		deviceID = generateDeviceID()
	}
	return &GQLClient{
		authToken: authToken,
		// 30s timeout on every Twitch request. Without this, a hung
		// connection (Twitch backend issues, DNS hiccup, mid-flight
		// reset) blocks the calling goroutine indefinitely. ProcessDrops,
		// Startup channel-resolve, Claim, CurrentDropSession poll all run
		// through this client — a stuck request would wedge the whole
		// drops loop until the process is killed.
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		deviceID:        deviceID,
		clientSessionID: generateSessionID(),
	}
}

// fetchTwitchUniqueID does what TDM does at auth: GET twitch.tv and read the
// `unique_id` cookie that Twitch sets in the response. Drop anti-cheat trusts
// IDs that Twitch itself issued — locally-generated random ones get flagged.
func fetchTwitchUniqueID() string {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "https://www.twitch.tv", nil)
	if err != nil {
		return ""
	}
	// Use the same Android UA as the rest of our requests.
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8*1024))
	for _, c := range resp.Cookies() {
		if c.Name == "unique_id" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// DeviceID returns the random 32-char fingerprint sent as X-Device-Id on GQL
// requests. Spade and other components reuse this so Twitch sees one session.
func (g *GQLClient) DeviceID() string {
	return g.deviceID
}

func (g *GQLClient) do(req *GQLRequest) (*GQLResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal gql request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", gqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	g.setHeaders(httpReq)

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gql request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxGQLBody))
	if err != nil {
		return nil, fmt.Errorf("read gql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gql status %d: %s", resp.StatusCode, string(respBody))
	}

	var gqlResp GQLResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("unmarshal gql response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return &gqlResp, fmt.Errorf("gql error: %s", gqlResp.Errors[0].Message)
	}

	return &gqlResp, nil
}

func (g *GQLClient) doBatch(reqs []GQLRequest) ([]GQLResponse, error) {
	body, err := json.Marshal(reqs)
	if err != nil {
		return nil, fmt.Errorf("marshal gql batch: %w", err)
	}

	httpReq, err := http.NewRequest("POST", gqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	g.setHeaders(httpReq)

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gql batch request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxGQLBody))
	if err != nil {
		return nil, fmt.Errorf("read gql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gql status %d: %s", resp.StatusCode, string(respBody))
	}

	var gqlResps []GQLResponse
	if err := json.Unmarshal(respBody, &gqlResps); err != nil {
		return nil, fmt.Errorf("unmarshal gql batch response: %w", err)
	}

	return gqlResps, nil
}

// GetUserInfo returns the logged-in user's info.
func (g *GQLClient) GetUserInfo() (*UserInfo, error) {
	req := &GQLRequest{
		Query: queryGetUserInfo,
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get user info: %w", err)
	}

	currentUser, ok := resp.Data["currentUser"]
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("invalid auth token or user not found")
	}

	userMap, ok := currentUser.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected user data format")
	}

	return &UserInfo{
		ID:          getString(userMap, "id"),
		Login:       getString(userMap, "login"),
		DisplayName: getString(userMap, "displayName"),
	}, nil
}

// GetChannelInfos resolves stream info for a batch of logins in parallel.
// Used by the drops selector to check online status of an ACL campaign's
// allowed_channels without going through the (often too small) game-directory
// top-100 — that's how TDM does it (services/channel_service.bulk_check_online).
//
// Logins are queried in goroutines; chunked to stay under typical GQL rate
// limits. Returns ChannelInfo entries only for channels that successfully
// resolved (network errors / not-found are silently skipped).
func (g *GQLClient) GetChannelInfos(logins []string) []*ChannelInfo {
	if len(logins) == 0 {
		return nil
	}
	const concurrency = 8
	type res struct {
		idx  int
		info *ChannelInfo
	}
	results := make([]*ChannelInfo, len(logins))
	sem := make(chan struct{}, concurrency)
	resCh := make(chan res, len(logins))
	for i, login := range logins {
		sem <- struct{}{}
		go func(idx int, login string) {
			defer func() { <-sem }()
			info, err := g.GetChannelInfo(login)
			if err != nil {
				resCh <- res{idx: idx, info: nil}
				return
			}
			resCh <- res{idx: idx, info: info}
		}(i, login)
	}
	for range logins {
		r := <-resCh
		results[r.idx] = r.info
	}
	return results
}

// GetChannelInfo returns channel info including live status.
func (g *GQLClient) GetChannelInfo(login string) (*ChannelInfo, error) {
	login = strings.ToLower(login)

	req := &GQLRequest{
		Query: queryGetChannelInfo,
		Variables: map[string]interface{}{
			"login": login,
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get channel info: %w", err)
	}

	info := &ChannelInfo{Login: login}

	user, ok := resp.Data["user"]
	if !ok || user == nil {
		return nil, fmt.Errorf("channel %q not found", login)
	}

	userMap, ok := user.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected channel data format")
	}

	info.ID = getString(userMap, "id")
	info.DisplayName = getString(userMap, "displayName")

	if stream, ok := userMap["stream"]; ok && stream != nil {
		if streamMap, ok := stream.(map[string]interface{}); ok {
			info.IsLive = true
			info.BroadcastID = getString(streamMap, "id")
			info.ViewerCount = getInt(streamMap, "viewersCount")
			if game, ok := streamMap["game"]; ok && game != nil {
				if gameMap, ok := game.(map[string]interface{}); ok {
					info.GameName = getString(gameMap, "displayName")
					info.GameID = getString(gameMap, "id")
				}
			}
		}
	}

	return info, nil
}

// GetChannelInfoByID resolves full channel info by ID (handles renames).
func (g *GQLClient) GetChannelInfoByID(channelID string) (*ChannelInfo, error) {
	req := &GQLRequest{
		Query: queryGetChannelInfoByID,
		Variables: map[string]interface{}{
			"id": channelID,
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get channel info by id: %w", err)
	}

	user, ok := resp.Data["user"]
	if !ok || user == nil {
		return nil, fmt.Errorf("channel ID %s not found", channelID)
	}

	userMap, ok := user.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected data format")
	}

	info := &ChannelInfo{
		ID:          getString(userMap, "id"),
		Login:       getString(userMap, "login"),
		DisplayName: getString(userMap, "displayName"),
	}

	if stream, ok := userMap["stream"]; ok && stream != nil {
		if streamMap, ok := stream.(map[string]interface{}); ok {
			info.IsLive = true
			info.BroadcastID = getString(streamMap, "id")
			info.ViewerCount = getInt(streamMap, "viewersCount")
			if game, ok := streamMap["game"]; ok && game != nil {
				if gameMap, ok := game.(map[string]interface{}); ok {
					info.GameName = getString(gameMap, "displayName")
					info.GameID = getString(gameMap, "id")
				}
			}
		}
	}

	return info, nil
}

// GetChannelNameByID resolves a channel's display name from its ID.
func (g *GQLClient) GetChannelNameByID(channelID string) (string, error) {
	req := &GQLRequest{
		Query: queryGetChannelNameByID,
		Variables: map[string]interface{}{
			"id": channelID,
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return "", fmt.Errorf("get channel name: %w", err)
	}

	user, ok := resp.Data["user"]
	if !ok || user == nil {
		return "", fmt.Errorf("channel %s not found", channelID)
	}

	userMap, ok := user.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("unexpected data format")
	}

	return getString(userMap, "displayName"), nil
}

// ErrClaimNotFound is returned (wrapped) by ClaimCommunityPoints when
// Twitch reports NOT_FOUND for a claim — meaning the claim was already
// consumed (manual click in the web UI, or claim window expired). It's
// a terminal failure for THIS claimID; retrying would never succeed.
// Callers should bail immediately via errors.Is(err, ErrClaimNotFound)
// instead of running the standard retry loop.
var ErrClaimNotFound = errors.New("claim not found")

// ClaimCommunityPoints claims a bonus chest.
func (g *GQLClient) ClaimCommunityPoints(channelID, claimID string) error {
	req := &GQLRequest{
		OperationName: "ClaimCommunityPoints",
		Query:         mutationClaimCommunityPoints,
		Variables: map[string]interface{}{
			"input": map[string]interface{}{
				"channelID": channelID,
				"claimID":   claimID,
			},
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// Check for claim-specific error in response data
	if resp != nil {
		if cpData, ok := resp.Data["claimCommunityPoints"]; ok && cpData != nil {
			if cpMap, ok := cpData.(map[string]interface{}); ok {
				if errData, ok := cpMap["error"]; ok && errData != nil {
					if errMap, ok := errData.(map[string]interface{}); ok {
						code := getString(errMap, "code")
						if code != "" {
							// NOT_FOUND is terminal — claim already consumed
							// or expired. Wrap the sentinel so callers can
							// errors.Is and skip the retry loop.
							if code == "NOT_FOUND" {
								return fmt.Errorf("claim rejected: %s: %w", code, ErrClaimNotFound)
							}
							return fmt.Errorf("claim rejected: %s", code)
						}
					}
				}
			}
		}
	}

	return nil
}

// JoinRaid joins an active raid. Tries persisted query hash first, falls back to raw mutation.
func (g *GQLClient) JoinRaid(raidID string) error {
	variables := map[string]interface{}{
		"input": map[string]interface{}{
			"raidID": raidID,
		},
	}

	// Try persisted query hash first (MinerV2 approach)
	req := &GQLRequest{
		OperationName: "JoinRaid",
		Variables:     variables,
		Extensions: &GQLExtensions{
			PersistedQuery: &PersistedQuery{
				Version:    1,
				SHA256Hash: joinRaidHash,
			},
		},
	}

	_, err := g.do(req)
	if err != nil {
		// Fallback to raw mutation
		req = &GQLRequest{
			OperationName: "JoinRaid",
			Query:         mutationJoinRaid,
			Variables:     variables,
		}
		_, err = g.do(req)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}

// GetChannelPointsBalance returns the current points balance for a channel.
func (g *GQLClient) GetChannelPointsBalance(channelLogin string) (int, error) {
	req := &GQLRequest{
		Query: queryChannelPointsBalance,
		Variables: map[string]interface{}{
			"channelLogin": strings.ToLower(channelLogin),
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return 0, fmt.Errorf("get points balance: %w", err)
	}

	community, ok := resp.Data["community"]
	if !ok || community == nil {
		// Try alternative path
		if ch, ok := resp.Data["channel"]; ok && ch != nil {
			if chMap, ok := ch.(map[string]interface{}); ok {
				if self, ok := chMap["self"]; ok && self != nil {
					if selfMap, ok := self.(map[string]interface{}); ok {
						if cp, ok := selfMap["communityPoints"]; ok && cp != nil {
							if cpMap, ok := cp.(map[string]interface{}); ok {
								return getInt(cpMap, "balance"), nil
							}
						}
					}
				}
			}
		}
		return 0, nil
	}

	if communityMap, ok := community.(map[string]interface{}); ok {
		if channel, ok := communityMap["channel"]; ok && channel != nil {
			if channelMap, ok := channel.(map[string]interface{}); ok {
				if self, ok := channelMap["self"]; ok && self != nil {
					if selfMap, ok := self.(map[string]interface{}); ok {
						if balance, ok := selfMap["balance"]; ok && balance != nil {
							if balMap, ok := balance.(map[string]interface{}); ok {
								return getInt(balMap, "availablePoints"), nil
							}
						}
					}
				}
			}
		}
	}

	return 0, nil
}

// GetGameStreams queries the game directory for live streams.
func (g *GQLClient) GetGameStreams(gameName string, limit int) ([]GameStream, error) {
	return g.fetchGameStreams(gameName, limit, queryGetGameStreams)
}

// GetGameStreamsDropsEnabled is like GetGameStreams but filters for streams
// that have the Drops Enabled system filter set — i.e. streamers actually
// running the drop campaign. Use this when auto-selecting a temp channel for
// an unrestricted drop campaign so we don't pick a streamer who is in the
// game category but not participating in drops.
func (g *GQLClient) GetGameStreamsDropsEnabled(gameName string, limit int) ([]GameStream, error) {
	return g.fetchGameStreams(gameName, limit, queryGetGameStreamsDropsEnabled)
}

// GetCurrentDropSession queries Twitch's DropCurrentSessionContext for the
// channel currently being watched. Returns the active drop's ID +
// currentMinutesWatched as Twitch sees them, or nil if no session exists yet.
// Used by the bot's heartbeat-loop polling — TwitchDropsMiner uses this same
// query as the bridge when user-drop-events PubSub is silent (which is often).
func (g *GQLClient) GetCurrentDropSession(channelID string) (*CurrentDropSession, error) {
	req := &GQLRequest{
		OperationName: "DropCurrentSessionContext",
		Variables: map[string]interface{}{
			"channelID":    channelID,
			"channelLogin": "",
		},
		Extensions: &GQLExtensions{
			PersistedQuery: &PersistedQuery{
				Version:    1,
				SHA256Hash: dropCurrentSessionHash,
			},
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("drop current session: %w", err)
	}

	cu, ok := resp.Data["currentUser"].(map[string]interface{})
	if !ok || cu == nil {
		return nil, nil
	}
	dcs, ok := cu["dropCurrentSession"].(map[string]interface{})
	if !ok || dcs == nil {
		return nil, nil
	}
	out := &CurrentDropSession{}
	if v, ok := dcs["dropID"].(string); ok {
		out.DropID = v
	}
	if v, ok := dcs["currentMinutesWatched"]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			out.CurrentMinutesWatched = int(n)
		case int:
			out.CurrentMinutesWatched = n
		}
	}
	if v, ok := dcs["requiredMinutesWatched"]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			out.RequiredMinutesWatched = int(n)
		case int:
			out.RequiredMinutesWatched = n
		}
	}
	if out.DropID == "" {
		return nil, nil
	}
	return out, nil
}

// SearchGameCategories proxies Twitch's searchCategories GQL — used for
// wanted-games autocomplete. Returns up to `limit` matching game category
// names (e.g. query="tarkov" -> ["Escape from Tarkov", "Escape from Tarkov: Arena", ...]).
func (g *GQLClient) SearchGameCategories(query string, limit int) ([]string, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	req := &GQLRequest{
		Query: `query SearchCategories($query: String!, $first: Int!) {
			searchCategories(query: $query, first: $first) {
				edges { node { id name } }
			}
		}`,
		Variables: map[string]interface{}{
			"query": query,
			"first": limit,
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("search categories: %w", err)
	}

	sc, ok := resp.Data["searchCategories"]
	if !ok || sc == nil {
		return nil, nil
	}
	scMap, ok := sc.(map[string]interface{})
	if !ok {
		return nil, nil
	}
	edgesRaw, ok := scMap["edges"]
	if !ok || edgesRaw == nil {
		return nil, nil
	}
	edges, ok := edgesRaw.([]interface{})
	if !ok {
		return nil, nil
	}

	var out []string
	for _, e := range edges {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		node, ok := em["node"].(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := node["name"].(string); ok && name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

func (g *GQLClient) fetchGameStreams(gameName string, limit int, query string) ([]GameStream, error) {
	req := &GQLRequest{
		OperationName: "DirectoryPage_Game",
		Query:         query,
		Variables: map[string]interface{}{
			"name":  gameName,
			"first": limit,
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get game streams: %w", err)
	}

	game, ok := resp.Data["game"]
	if !ok || game == nil {
		return nil, nil
	}
	gameMap, ok := game.(map[string]interface{})
	if !ok {
		return nil, nil
	}

	streams, ok := gameMap["streams"]
	if !ok || streams == nil {
		return nil, nil
	}
	streamsMap, ok := streams.(map[string]interface{})
	if !ok {
		return nil, nil
	}

	edges, ok := streamsMap["edges"]
	if !ok || edges == nil {
		return nil, nil
	}
	edgeList, ok := edges.([]interface{})
	if !ok {
		return nil, nil
	}

	var result []GameStream
	for _, e := range edgeList {
		eMap, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		node, ok := eMap["node"]
		if !ok || node == nil {
			continue
		}
		nMap, ok := node.(map[string]interface{})
		if !ok {
			continue
		}

		gs := GameStream{
			ViewerCount: getInt(nMap, "viewersCount"),
		}

		if broadcaster, ok := nMap["broadcaster"]; ok && broadcaster != nil {
			if bMap, ok := broadcaster.(map[string]interface{}); ok {
				gs.BroadcasterID = getString(bMap, "id")
				gs.BroadcasterLogin = getString(bMap, "login")
				gs.DisplayName = getString(bMap, "displayName")
			}
		}
		if game, ok := nMap["game"]; ok && game != nil {
			if gMap, ok := game.(map[string]interface{}); ok {
				gs.GameID = getString(gMap, "id")
				gs.GameName = getString(gMap, "name")
			}
		}

		if gs.BroadcasterLogin != "" {
			result = append(result, gs)
		}
	}

	return result, nil
}

// setHeaders sets all required headers for TV Client-ID GQL requests.
// Header set matches TDM (TwitchDropsMiner) gql_client.py exactly — captured
// from a working TDM session on the same account. Drop anti-cheat checks
// Origin + Referer in addition to client-id/UA correlation; without these
// the sendSpadeEvents mutation returns 204 but the credit pipeline ignores it.
func (g *GQLClient) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	// NOTE: Don't set Accept-Encoding manually — Go's transport handles gzip
	// transparently only when WE don't set the header. Setting it ourselves
	// returns a raw gzipped body that breaks json.Unmarshal everywhere.
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Client-Id", TVClientID)
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Client-Session-Id", g.clientSessionID)
	req.Header.Set("X-Device-Id", g.deviceID)
	req.Header.Set("Origin", "https://www.twitch.tv")
	req.Header.Set("Referer", "https://www.twitch.tv")
	req.Header.Set("Authorization", "OAuth "+g.authToken)
	req.Header.Set("Content-Type", "application/json")
}

// generateDeviceID returns 32 random alphanumeric characters for X-Device-Id.
func generateDeviceID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 32)
	rand.Read(b)
	for i := range b {
		b[i] = chars[b[i]%byte(len(chars))]
	}
	return string(b)
}

// generateSessionID returns 32 hex characters (16 random bytes) for Client-Session-Id.
func generateSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Helper to safely get a string from a map.
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Helper to safely get an int from a map (JSON numbers are float64).
func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}
