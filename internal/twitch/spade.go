package twitch

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
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

// SpadeTracker sends minute-watched heartbeats. As of 2024+ Twitch only
// credits drops via the `sendSpadeEvents` GQL mutation — the legacy
// POST to spade.twitch.tv/track silently fails on stricter campaigns.
// We call gql.SendMinuteWatched here. Name kept for compatibility.
type SpadeTracker struct {
	userID     string
	authToken  string
	deviceID   string // kept for legacy fallback; no longer used by GQL path
	spadeURL   string // legacy, only used as informational log
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
// Returns false if at max capacity.
func (s *SpadeTracker) StartWatching(channelID, channelLogin, broadcastID, gameName, gameID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

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

func (s *SpadeTracker) sendHeartbeat(ch *spadeChannel) {
	for attempt := range heartbeatMaxRetries + 1 {
		err := s.gql.SendMinuteWatched(ch.channelID, ch.channelLogin, ch.broadcastID, ch.gameName, ch.gameID, s.userID)
		if err == nil {
			return
		}
		if attempt < heartbeatMaxRetries {
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}
		s.log("[Spade] heartbeat for %s failed after %d attempts: %v", ch.channelLogin, attempt+1, err)
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

	// Step 2: Extract settings JS bundle URL from the page
	settingsRe := regexp.MustCompile(`(https://static\.twitchcdn\.net/config/settings\.[^"'\s]+\.js)`)
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
func (s *SpadeTracker) UpdateBroadcastID(channelID, broadcastID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.channels[channelID]; ok {
		ch.broadcastID = broadcastID
	}
}
