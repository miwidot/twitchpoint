package twitch

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// StreamProber periodically fetches the HLS playlist + a chunk for each
// channel the bot is "watching" via Spade. This is required because some
// drop campaigns (notably ABI Partner-Only and other anti-cheat-flagged
// ones) only credit minutes when Twitch sees actual stream-chunk requests
// against the user's session — pure Spade heartbeats are silently rejected.
//
// TwitchDropsMiner has the same code in channel.py:_send_watch (currently
// unused there). We turn it on for every pick.
//
// Bandwidth: ~5 KB / channel / minute (m3u8 + chunk-list, no actual video).

const (
	// The browser player fetches a new HLS chunk every ~2-6s. Twitch's drop
	// anti-cheat appears to flag long gaps (>10s) as "user not watching" and
	// refuses to initialize a drop session. 12s gives 5 probes/min — close to
	// the browser's request rate without being overly chatty.
	proberInterval = 10 * time.Second
	tokenCacheTTL  = 50 * time.Minute
	// Cap the chunk read at 256 KB. Real chunks are 200-800 KB; reading the
	// first 256 KB is enough to "consume" the segment per Twitch's tracking
	// (their CDN logs the byte range you request).
	chunkReadLimit = 256 * 1024
	usherURLFmt    = "https://usher.ttvnw.net/api/channel/hls/%s.m3u8?sig=%s&token=%s&allow_source=true&fast_bread=true&player_version=1.31.0&platform=web&supported_codecs=h264&player_backend=mediaplayer&playlist_include_framerate=true"
)

type playbackToken struct {
	value     string
	signature string
	fetchedAt time.Time
}

type proberChannel struct {
	login  string
	stopCh chan struct{}

	// seenSegments deduplicates media segments per channel. Twitch counts
	// DISTINCT consumed segments, so re-requesting the same one is worth
	// nothing — and skipping the ones in between is what starves the
	// drop counter. Bounded via seenOrder so a long session cannot grow
	// this without limit.
	seenSegments map[string]struct{}
	seenOrder    []string
}

type StreamProber struct {
	gql        *GQLClient
	authToken  string
	userID     string
	deviceID   string
	httpClient *http.Client
	logFunc    func(string, ...interface{})

	mu       sync.Mutex
	tokens   map[string]*playbackToken
	channels map[string]*proberChannel
	stopCh   chan struct{}
	stopped  bool
}

func NewStreamProber(gql *GQLClient, authToken, userID, deviceID string, logFunc func(string, ...interface{})) *StreamProber {
	return &StreamProber{
		gql:        gql,
		authToken:  authToken,
		userID:     userID,
		deviceID:   deviceID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		logFunc:    logFunc,
		tokens:     make(map[string]*playbackToken),
		channels:   make(map[string]*proberChannel),
		stopCh:     make(chan struct{}),
	}
}

// Start begins probing the channel. No-op if already probing or after StopAll.
func (p *StreamProber) Start(login string) {
	login = strings.ToLower(login)
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if _, ok := p.channels[login]; ok {
		p.mu.Unlock()
		return
	}
	ch := &proberChannel{
		login:        login,
		stopCh:       make(chan struct{}),
		seenSegments: make(map[string]struct{}),
	}
	p.channels[login] = ch
	p.mu.Unlock()

	go p.probeLoop(ch)
}

// Stop cancels probing for the channel.
func (p *StreamProber) Stop(login string) {
	login = strings.ToLower(login)
	p.mu.Lock()
	ch, ok := p.channels[login]
	if ok {
		delete(p.channels, login)
	}
	p.mu.Unlock()
	if ok {
		close(ch.stopCh)
	}
}

// StopAll cancels every running prober and prevents new ones.
func (p *StreamProber) StopAll() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stopCh)
	for login, ch := range p.channels {
		close(ch.stopCh)
		delete(p.channels, login)
	}
	p.mu.Unlock()
}

func (p *StreamProber) probeLoop(ch *proberChannel) {
	p.log("[Prober] %s started", ch.login)
	// Visit the channel page like TDM does in get_spade_url() — that GET on
	// twitch.tv/<login> is the page-view that primes Twitch's drop-session
	// state for this user/channel pair. Without it the heartbeats arrive at
	// beacon but no session exists to credit them against.
	p.primeChannelPage(ch.login)
	p.probeOnce(ch.login)

	ticker := time.NewTicker(proberInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.probeOnce(ch.login)
		case <-ch.stopCh:
			return
		case <-p.stopCh:
			return
		}
	}
}

func (p *StreamProber) probeOnce(login string) {
	tok, err := p.getToken(login)
	if err != nil {
		p.log("[Prober] %s token fetch failed: %v", login, err)
		return
	}

	playlistURL := fmt.Sprintf(usherURLFmt, url.PathEscape(login), url.QueryEscape(tok.signature), url.QueryEscape(tok.value))

	body, status, ok := p.fetchText(playlistURL, 64*1024)
	if !ok {
		if status == http.StatusForbidden || status == http.StatusUnauthorized || status == http.StatusBadRequest {
			p.invalidateToken(login)
			p.log("[Prober] %s playlist HTTP %d, token invalidated", login, status)
		} else if status == http.StatusNotFound {
			// Stream went offline — stop probing this channel; farmer will
			// call Start again when stream comes back.
			p.log("[Prober] %s stream offline (404)", login)
		}
		return
	}

	chunkPlaylistURL := pickLowestQualityVariant(body)
	if chunkPlaylistURL == "" {
		return
	}

	chunkBody, _, ok := p.fetchText(chunkPlaylistURL, 32*1024)
	if !ok {
		return
	}

	segments := mediaSegments(chunkBody)
	if len(segments) == 0 {
		return
	}

	// Consume EVERY segment we have not already requested. Twitch credits
	// distinct consumed segments: fetching only the newest one (what this
	// did before) consumes roughly one segment per poll while the stream
	// publishes several, which left the drop counter frozen for hours.
	// Re-requesting a segment we already took is worth nothing, hence the
	// per-channel dedup.
	fresh := p.unseenSegments(login, segments)
	if len(fresh) == 0 {
		return // playlist has not advanced yet; nothing new to consume
	}

	// HEAD on each new segment is what upstream verified restores credit
	// (rangermix/TwitchDropsMiner#164) and costs almost no bandwidth. The
	// newest segment additionally gets a byte-capped GET, preserving the
	// "real bytes downloaded" behaviour that has always worked here —
	// cheap insurance in case HEAD alone stops counting again.
	headed := 0
	for _, seg := range fresh {
		if p.requestSegment(seg, http.MethodHead) {
			headed++
		}
	}

	newest := fresh[len(fresh)-1]
	chunkReq, err := http.NewRequest(http.MethodGet, newest, nil)
	if err != nil {
		return
	}
	p.setPlayerHeaders(chunkReq)
	chunkResp, err := p.httpClient.Do(chunkReq)
	if err != nil {
		p.log("[Prober] %s chunk GET failed: %v", login, err)
		return
	}
	bytesRead, _ := io.Copy(io.Discard, io.LimitReader(chunkResp.Body, chunkReadLimit))
	chunkResp.Body.Close()
	p.log("[Prober] %s probe ok (%d new segment(s), %d HEAD ok, chunk HTTP %d, %d bytes)",
		login, len(fresh), headed, chunkResp.StatusCode, bytesRead)
}

// setPlayerHeaders makes a segment request look like the web player's.
func (p *StreamProber) setPlayerHeaders(req *http.Request) {
	req.Header.Set("User-Agent", ActiveClient().UserAgent)
	req.Header.Set("Origin", "https://www.twitch.tv")
	req.Header.Set("Referer", "https://www.twitch.tv/")
}

// requestSegment issues one segment request and reports whether Twitch
// accepted it. Bodies are never read — HEAD carries no payload, and the
// caller downloads bytes separately for the newest segment only.
func (p *StreamProber) requestSegment(segURL, method string) bool {
	req, err := http.NewRequest(method, segURL, nil)
	if err != nil {
		return false
	}
	p.setPlayerHeaders(req)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// maxSeenSegments bounds the per-channel dedup cache. A Twitch live media
// playlist advertises well under a minute of segments, so a few hundred
// covers any realistic poll gap while keeping the memory flat over a
// multi-hour session.
const maxSeenSegments = 300

// unseenSegments returns the segments not yet requested for this channel
// and records them as seen. Order is preserved so the caller can treat the
// last entry as the newest.
func (p *StreamProber) unseenSegments(login string, segments []string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.channels[strings.ToLower(login)]
	if !ok {
		return nil // channel stopped while we were fetching
	}
	if ch.seenSegments == nil {
		ch.seenSegments = make(map[string]struct{})
	}

	var fresh []string
	for _, seg := range segments {
		if _, seen := ch.seenSegments[seg]; seen {
			continue
		}
		ch.seenSegments[seg] = struct{}{}
		ch.seenOrder = append(ch.seenOrder, seg)
		fresh = append(fresh, seg)
	}

	// Trim oldest entries past the bound.
	if excess := len(ch.seenOrder) - maxSeenSegments; excess > 0 {
		for _, old := range ch.seenOrder[:excess] {
			delete(ch.seenSegments, old)
		}
		ch.seenOrder = ch.seenOrder[excess:]
	}
	return fresh
}

func (p *StreamProber) fetchText(u string, limit int64) (string, int, bool) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("User-Agent", ActiveClient().UserAgent)
	req.Header.Set("Origin", "https://www.twitch.tv")
	req.Header.Set("Referer", "https://www.twitch.tv/")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", 0, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", resp.StatusCode, false
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), resp.StatusCode, false
	}
	return string(body), resp.StatusCode, true
}

func (p *StreamProber) getToken(login string) (*playbackToken, error) {
	p.mu.Lock()
	if tok, ok := p.tokens[login]; ok && time.Since(tok.fetchedAt) < tokenCacheTTL {
		p.mu.Unlock()
		return tok, nil
	}
	p.mu.Unlock()

	val, sig, err := p.gql.GetPlaybackAccessToken(login)
	if err != nil {
		return nil, err
	}
	tok := &playbackToken{
		value:     val,
		signature: sig,
		fetchedAt: time.Now(),
	}
	p.mu.Lock()
	p.tokens[login] = tok
	p.mu.Unlock()
	return tok, nil
}

func (p *StreamProber) invalidateToken(login string) {
	p.mu.Lock()
	delete(p.tokens, login)
	p.mu.Unlock()
}

func (p *StreamProber) log(format string, args ...interface{}) {
	if p.logFunc != nil {
		p.logFunc(format, args...)
	}
}

// primeChannelPage GETs twitch.tv/<login> to register a "page view" with
// Twitch's tracking system. Browsers do this on navigation; some drop
// campaigns require it before the drop-credit subsystem creates a session.
func (p *StreamProber) primeChannelPage(login string) {
	url := "https://www.twitch.tv/" + login
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", ActiveClient().UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cookie", fmt.Sprintf("auth-token=%s; persistent=%s; unique_id=%s",
		p.authToken, p.userID, p.deviceID))
	resp, err := p.httpClient.Do(req)
	if err != nil {
		p.log("[Prober] %s page-view failed: %v", login, err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8*1024)) // discard, only need the request itself
	resp.Body.Close()
	p.log("[Prober] %s page-view ok (HTTP %d)", login, resp.StatusCode)
}

// mediaSegments returns every media segment URL in a media playlist, in
// playback order. The previous implementation took only the LAST segment,
// which meant one segment consumed per poll while the stream published
// several — Twitch credits distinct consumed segments, so the drop counter
// crawled to a standstill (stuck at 3/60 for hours on 2026-10-09).
func mediaSegments(playlist string) []string {
	var out []string
	for _, raw := range strings.Split(playlist, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "https://") {
			continue
		}
		if strings.Contains(line, ".m3u8") {
			continue
		}
		if strings.Contains(line, "/segment/") {
			out = append(out, line)
		}
	}
	return out
}

// pickLowestQualityVariant returns the URL of the lowest-bandwidth variant
// (audio-only when available, else the lowest quality video) in an HLS
// master playlist. The bot doesn't care about quality — only that Twitch
// sees a chunk request.
func pickLowestQualityVariant(playlist string) string {
	lines := strings.Split(playlist, "\n")
	// Audio-only variant URLs typically appear near the end of the master
	// playlist; iterate backwards and return the first https:// line found.
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}
