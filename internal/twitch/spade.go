package twitch

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Modern Twitch tracking endpoint. The legacy spade.twitch.tv/track URL
	// still accepts requests (returns 204) but the drop-credit pipeline only
	// honors heartbeats sent to beacon.twitch.tv. TDM uses this endpoint via
	// the `beacon_?url` regex on settings.js — see TDM channel.py:300.
	spadeURLFallback   = "https://beacon.twitch.tv/track"
	heartbeatInterval  = 60 * time.Second
	maxWatchedChannels = 2
	// MUST match the client-ID we use (kd1unb4b3q4t58fwlpcbzcbnm76a8fp = Android App).
	// Twitch's drop anti-cheat correlates client-ID with user-agent — sending
	// Android client-ID with a Windows Chrome UA gets flagged and silently
	// blocks drop credit (channel-points still credit because that uses a
	// different verification path). TDM uses identical Dalvik UAs for this
	// client-ID; see TDM config/client_info.py ClientType.ANDROID_APP.
	browserUserAgent = "Dalvik/2.1.0 (Linux; U; Android 16; SM-S911B Build/TP1A.220624.014) tv.twitch.android.app/25.3.0/2503006"
)

// SpadeTracker sends minute-watched heartbeats for watch credit. It
// POSTs the legacy event payload to the beacon/spade endpoint resolved
// at Start() (see fetchSpadeURL). Since 2026-07-10 this pipeline carries
// BOTH channel-points WATCH credit AND drop-minute credit (Twitch killed
// crediting via the sendSpadeEvents GQL mutation, so the drops pick now
// holds a heartbeat slot here too — see drops/apply.go step 8).
// See sendHeartbeat for the long-form pipeline rationale.
type SpadeTracker struct {
	userID     string
	authToken  string
	deviceID   string // kept for legacy fallback; no longer used by GQL path
	spadeURL   string // POST target for channel-points heartbeats; resolved at Start()
	gql        *GQLClient
	httpClient *http.Client
	logFunc    func(string, ...interface{})

	mu       sync.Mutex
	channels map[string]*spadeChannel // channelID -> channel
	stopCh   chan struct{}
	stopped  bool
}

type spadeChannel struct {
	channelID    string
	channelLogin string
	broadcastID  string
	gameName     string
	gameID       string
	stopCh       chan struct{}
}

// NewSpadeTracker creates a new Spade tracker for sending watch heartbeats.
// gql is the shared GQL client — heartbeats are sent via the sendSpadeEvents
// mutation through it (the only credit-honored path on modern Twitch).
func NewSpadeTracker(userID, authToken, deviceID string, gql *GQLClient, logFunc func(string, ...interface{})) *SpadeTracker {
	return &SpadeTracker{
		userID:     userID,
		authToken:  authToken,
		deviceID:   deviceID,
		gql:        gql,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logFunc:    logFunc,
		channels:   make(map[string]*spadeChannel),
		stopCh:     make(chan struct{}),
	}
}

// Start initializes the Spade tracker and fetches the Spade URL.
func (s *SpadeTracker) Start() error {
	spadeURL, err := s.fetchSpadeURL()
	if err != nil {
		s.spadeURL = spadeURLFallback
	} else {
		s.spadeURL = spadeURL
	}
	s.log("[Spade] using URL: %s", s.spadeURL)
	return err
}

// StartWatching begins sending heartbeats for a channel.
// Returns false if at max capacity OR after Stop() has been called.
func (s *SpadeTracker) StartWatching(channelID, channelLogin, broadcastID, gameName, gameID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reject late callers after Stop. Without this guard, a rotation
	// or fetch goroutine that races with Farmer.Stop() could re-add a
	// channel + spawn a heartbeatLoop AFTER Stop already drained the
	// map and closed s.stopCh. The new goroutine would exit on its
	// first select-on-stopCh iteration, but only after sending one
	// stray heartbeat (heartbeatLoop sends immediately before
	// entering the ticker loop).
	if s.stopped {
		return false
	}

	// Already watching this channel
	if _, ok := s.channels[channelID]; ok {
		// Update broadcast ID and game in case they changed
		s.channels[channelID].broadcastID = broadcastID
		s.channels[channelID].gameName = gameName
		s.channels[channelID].gameID = gameID
		return true
	}

	// Check capacity
	if len(s.channels) >= maxWatchedChannels {
		return false
	}

	ch := &spadeChannel{
		channelID:    channelID,
		channelLogin: channelLogin,
		broadcastID:  broadcastID,
		gameName:     gameName,
		gameID:       gameID,
		stopCh:       make(chan struct{}),
	}
	s.channels[channelID] = ch

	go s.heartbeatLoop(ch)
	return true
}

// StopWatching stops sending heartbeats for a channel.
func (s *SpadeTracker) StopWatching(channelID string) {
	s.mu.Lock()
	ch, ok := s.channels[channelID]
	if ok {
		delete(s.channels, channelID)
	}
	s.mu.Unlock()

	if ok {
		close(ch.stopCh)
	}
}

// IsWatching returns whether a channel is being actively watched.
func (s *SpadeTracker) IsWatching(channelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.channels[channelID]
	return ok
}

// WatchedCount returns the number of actively watched channels.
func (s *SpadeTracker) WatchedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.channels)
}

// ActiveSlots returns remaining watch slots.
func (s *SpadeTracker) ActiveSlots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maxWatchedChannels - len(s.channels)
}

// Stop shuts down all heartbeat loops.
func (s *SpadeTracker) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stopCh)

	for id, ch := range s.channels {
		close(ch.stopCh)
		delete(s.channels, id)
	}
	s.mu.Unlock()
}

func (s *SpadeTracker) heartbeatLoop(ch *spadeChannel) {
	// Send first heartbeat immediately
	s.sendHeartbeat(ch)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.sendHeartbeat(ch)
		case <-ch.stopCh:
			return
		case <-s.stopCh:
			return
		}
	}
}

const heartbeatMaxRetries = 2

// sendHeartbeat posts the minute-watched event for watch credit.
//
// Pipeline history:
//
//   - Pre-2026-07-10, Twitch had TWO separate credit pipelines: channel-
//     points WATCH via this POST endpoint (`player: "site"` /
//     `location: "channel"`), drop minutes via the `sendSpadeEvents`
//     GraphQL mutation (drops.Watcher's path).
//   - 2026-07-10: Twitch stopped crediting drop minutes via the GQL
//     mutation entirely (DevilXD/TwitchDropsMiner#1099 broke the same
//     day). Drop credit now ALSO flows through this POST pipeline — but
//     only when the payload carries the game attribution fields (`game`,
//     `game_id`) and an INT `user_id`. A real browser has always sent
//     these fields, and channel-points credit is unaffected by them.
//     Independently confirmed by INKCR0W/TwitchDropsMinerGo commits
//     f590d9b + 63b3287 (2026-07-10): their minimal payload got 204
//     without crediting; adding game/game_id made drops credit.
//
// Restored 2026-04-29 after the v2.0 ABI fix accidentally collapsed both
// pipelines onto sendSpadeEvents — drops kept crediting, but channel-
// points went silent for hours until the user noticed (real Twitch web
// session re-credited cpt_blackshark immediately, confirming the bot
// alone wasn't reaching the WATCH-credit pipeline).
func (s *SpadeTracker) sendHeartbeat(ch *spadeChannel) {
	// Snapshot the mutable fields under s.mu. UpdateBroadcastID and
	// StartWatching write to ch.broadcastID/gameName/gameID under the
	// same lock; without snapshotting we'd race against them on every
	// heartbeat. channelID/channelLogin are technically write-once (set
	// in StartWatching, never mutated) but we snapshot them too so the
	// payload assembly works on a consistent struct.
	s.mu.Lock()
	channelID := ch.channelID
	channelLogin := ch.channelLogin
	broadcastID := ch.broadcastID
	gameName := ch.gameName
	gameID := ch.gameID
	s.mu.Unlock()

	// INT user_id, not string — same rule as the GQL variant (gql.go):
	// Twitch's drop-credit pipeline validates the type; a string user_id
	// returns 204 but the credit is silently dropped.
	uidInt, err := strconv.ParseInt(s.userID, 10, 64)
	if err != nil {
	return
	}

	payload := []map[string]interface{}{
		{
			"event": "minute-watched",
			"properties": map[string]interface{}{
				"channel_id":     channelID,
				"broadcast_id":   broadcastID,
				"player":         "site",
				"user_id":        uidInt,
				"channel":        channelLogin,
				"client_time":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				"game":           gameName,
				"game_id":        gameID,
				"hidden":         false,
				"is_live":        true,
				"live":           true,
				"location":       "channel",
				"logged_in":      true,
				"minutes_logged": 1,
				"muted":          false,
			},
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return
	}

	encoded := base64.StdEncoding.EncodeToString(jsonData)
	body := url.Values{"data": {encoded}}.Encode()

	for attempt := range heartbeatMaxRetries + 1 {
		req, err := http.NewRequest("POST", s.spadeURL, strings.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", browserUserAgent)

		resp, err := s.httpClient.Do(req)
		if err != nil {
			if attempt < heartbeatMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
				continue
			}
			s.log("[Spade] heartbeat failed for %s after %d attempts: %v", channelLogin, attempt+1, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		// Per TDM (channel.py:483): only 204 means accepted. Twitch
		// returns 200 with an error body when the heartbeat is
		// technically valid but the credit subsystem rejected it
		// (anti-cheat). Treating that as success would mask failures.
		if resp.StatusCode == http.StatusNoContent {
			return
		}
		if attempt < heartbeatMaxRetries {
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}
		s.log("[Spade] heartbeat for %s returned HTTP %d after %d attempts", channelLogin, resp.StatusCode, attempt+1)
		return
	}
}

func (s *SpadeTracker) log(format string, args ...interface{}) {
	if s.logFunc != nil {
		s.logFunc(format, args...)
	}
}

func (s *SpadeTracker) fetchSpadeURL() (string, error) {
	// Step 1: Fetch Twitch page to find the settings JS URL
	req, err := http.NewRequest("GET", "https://www.twitch.tv", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch twitch page: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read twitch page: %w", err)
	}

	pageBody := string(bodyBytes)

	// Look for beacon_url first (modern, drop-credit honored), spade_url as
	// secondary. TDM channel.py:300 uses `beacon_?url` for the same reason —
	// only beacon heartbeats actually credit drops on stricter campaigns.
	spadePatterns := []string{
		`"beacon_url"\s*:\s*"(https://[^"]+)"`,
		`"beaconUrl"\s*:\s*"(https://[^"]+)"`,
		`"spade_url"\s*:\s*"(https://[^"]+)"`,
		`"spadeUrl"\s*:\s*"(https://[^"]+)"`,
	}
	for _, p := range spadePatterns {
		re := regexp.MustCompile(p)
		matches := re.FindStringSubmatch(pageBody)
		if len(matches) >= 2 {
			return matches[1], nil
		}
	}

	// Step 2: Extract settings JS bundle URL from the page. Twitch moved the
	// host from static.twitchcdn.net to assets.twitch.tv around 2026-04 — we
	// match both so we keep working if they flip back (or A/B test it).
	settingsRe := regexp.MustCompile(`(https://(?:static\.twitchcdn\.net|assets\.twitch\.tv)/config/settings\.[^"'\s]+\.js)`)
	settingsMatch := settingsRe.FindStringSubmatch(pageBody)
	if len(settingsMatch) < 2 {
		return "", fmt.Errorf("settings JS URL not found in page")
	}

	// Step 3: Fetch the settings JS and extract spade_url from it
	settingsReq, err := http.NewRequest("GET", settingsMatch[1], nil)
	if err != nil {
		return "", fmt.Errorf("create settings request: %w", err)
	}
	settingsReq.Header.Set("User-Agent", browserUserAgent)

	settingsResp, err := s.httpClient.Do(settingsReq)
	if err != nil {
		return "", fmt.Errorf("fetch settings JS: %w", err)
	}
	defer settingsResp.Body.Close()

	settingsBytes, err := io.ReadAll(settingsResp.Body)
	if err != nil {
		return "", fmt.Errorf("read settings JS: %w", err)
	}

	settingsBody := string(settingsBytes)

	for _, p := range spadePatterns {
		re := regexp.MustCompile(p)
		matches := re.FindStringSubmatch(settingsBody)
		if len(matches) >= 2 {
			return matches[1], nil
		}
	}

	return "", fmt.Errorf("spade URL not found in settings JS")
}

// UpdateBroadcastID updates the broadcast ID for an already-watched channel.
func (s *SpadeTracker) UpdateBroadcastID(channelID, broadcastID, gameName, gameID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.channels[channelID]; ok {
		ch.broadcastID = broadcastID
		// Also refresh game attribution — a mid-session game change must
		// reach the heartbeat payload, otherwise Twitch keeps seeing the
		// stale game_id and silently drops the drop credit. Guard against
		// empty values so a partial refresh can't wipe a good game_id.
		if gameName != "" {
			ch.gameName = gameName
		}
		if gameID != "" {
			ch.gameID = gameID
		}
	}
}
