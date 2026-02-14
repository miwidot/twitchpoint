package twitch

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	spadeURLFallback   = "https://spade.twitch.tv/track"
	heartbeatInterval  = 60 * time.Second
	maxWatchedChannels = 2
	browserUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// SpadeTracker sends minute-watched heartbeats to Twitch's Spade endpoint.
type SpadeTracker struct {
	userID     string
	authToken  string
	spadeURL   string
	httpClient *http.Client
	logFunc    func(string, ...interface{})

	mu       sync.Mutex
	channels map[string]*spadeChannel // channelID -> channel
	stopCh   chan struct{}
	stopped  bool
}

type spadeChannel struct {
	channelID   string
	channelLogin string
	broadcastID string
	stopCh      chan struct{}
}

// NewSpadeTracker creates a new Spade tracker for sending watch heartbeats.
func NewSpadeTracker(userID, authToken string, logFunc func(string, ...interface{})) *SpadeTracker {
	return &SpadeTracker{
		userID:     userID,
		authToken:  authToken,
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
func (s *SpadeTracker) StartWatching(channelID, channelLogin, broadcastID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already watching this channel
	if _, ok := s.channels[channelID]; ok {
		// Update broadcast ID if changed
		s.channels[channelID].broadcastID = broadcastID
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
	payload := []map[string]interface{}{
		{
			"event": "minute-watched",
			"properties": map[string]interface{}{
				"channel_id":   ch.channelID,
				"broadcast_id": ch.broadcastID,
				"player":       "site",
				"user_id":      s.userID,
				"live":         true,
				"channel":      ch.channelLogin,
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
			s.log("[Spade] heartbeat failed for %s after %d attempts: %v", ch.channelLogin, attempt+1, err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
			return
		}
		if attempt < heartbeatMaxRetries {
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}
		s.log("[Spade] heartbeat for %s returned HTTP %d after %d attempts", ch.channelLogin, resp.StatusCode, attempt+1)
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

	// Try to find spade URL directly in the HTML first
	spadePatterns := []string{
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
