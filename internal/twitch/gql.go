package twitch

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	gqlURL     = "https://gql.twitch.tv/gql"
	TVClientID = "ue6666qo983tsx6so1t0vnawi233wa" // TV/Android client-ID — bypasses integrity token requirement
)

// Raw GQL queries for read operations
const (
	queryGetUserInfo = `query { currentUser { id login displayName } }`

	queryGetChannelInfo = `query GetChannelInfo($login: String!) {
		user(login: $login) {
			id login displayName
			stream { id viewersCount game { displayName } }
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
					}
				}
			}
		}
	}`

	queryGetChannelNameByID = `query GetChannelNameByID($id: ID!) {
		user(id: $id) { displayName }
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
)

// GQLClient handles all Twitch GQL API calls.
type GQLClient struct {
	authToken       string
	httpClient      *http.Client
	deviceID        string // X-Device-Id header (32 alphanumeric, persisted per session)
	clientSessionID string // Client-Session-Id header (16 hex bytes, per session)
}

// NewGQLClient creates a new GQL client with the given auth token.
func NewGQLClient(authToken string) *GQLClient {
	return &GQLClient{
		authToken:       authToken,
		httpClient:      &http.Client{},
		deviceID:        generateDeviceID(),
		clientSessionID: generateSessionID(),
	}
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

	respBody, err := io.ReadAll(resp.Body)
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

	respBody, err := io.ReadAll(resp.Body)
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
	req := &GQLRequest{
		OperationName: "DirectoryPage_Game",
		Query:         queryGetGameStreams,
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

		if gs.BroadcasterLogin != "" {
			result = append(result, gs)
		}
	}

	return result, nil
}

// setHeaders sets all required headers for TV Client-ID GQL requests.
func (g *GQLClient) setHeaders(req *http.Request) {
	req.Header.Set("Client-Id", TVClientID)
	req.Header.Set("Authorization", "OAuth "+g.authToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", g.deviceID)
	req.Header.Set("Client-Session-Id", g.clientSessionID)
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
