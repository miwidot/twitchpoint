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
// Since ~2026-10-05 Twitch only credits a watched minute when it has seen
// requests for (nearly) every media segment of that minute, not just one
// chunk every few seconds (rangermix/TwitchDropsMiner#163, PR #164). So on
// every tick we read the media playlist and HEAD every segment we have not
// touched yet — ~30 requests/min, headers only, no audio/video bodies. The
// capped GET on the newest segment is kept as belt-and-braces for campaigns
// whose anti-cheat wants to see actual bytes.
//
// Bandwidth: ~5 KB / channel / minute for playlists + HEADs, plus one
// capped chunk GET (<=256 KB) per tick.

const (
	// Twitch's low-latency media playlists hold a rolling window of ~2s
	// segments. Polling every 10s stays well inside that window, so every
	// segment of the stream is seen at least once (the HEAD loop below
	// deduplicates the overlap).
	proberInterval = 10 * time.Second
	tokenCacheTTL  = 50 * time.Minute
	// Resolve the media playlist URL from usher only every few minutes; the
	// signed URL stays valid for a while and is refreshed on any 4xx.
	mediaPlaylistTTL = 4 * time.Minute
	// Bounded dedup cache of successfully HEADed segments per channel.
	seenSegmentsCap = 256
	// Per-segment HEAD timeout; one stalled CDN edge must not block the tick.
	segmentHeadTimeout = 3 * time.Second
	// Hard cap on HEADs per tick (first poll after a restart sees the whole
	// window at once; a stale playlist could hold more).
	maxSegmentHeadsPerTick = 64
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

	// Prober-goroutine-private state (only touched from probeLoop).
	mediaURL     string    // cached media (chunk) playlist URL
	mediaFetched time.Time // when mediaURL was resolved from usher
	seen         map[string]struct{}
	seenOrder    []string // insertion order for bounded eviction
}

type StreamProber struct {
	gql        *GQLClient
	authToken  string
	userID     string
	deviceID   string
	httpClient *http.Client
	headClient *http.Client // short timeout, used for per-segment HEADs
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
		headClient: &http.Client{Timeout: segmentHeadTimeout},
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
		login:  login,
		stopCh: make(chan struct{}),
		seen:   make(map[string]struct{}),
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
	p.probeOnce(ch)

	ticker := time.NewTicker(proberInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.probeOnce(ch)
		case <-ch.stopCh:
			return
		case <-p.stopCh:
			return
		}
	}
}

// probeOnce runs one tick: make sure we hold a media playlist URL, read the
// playlist, HEAD every segment not seen before and GET (capped) the newest
// one.
func (p *StreamProber) probeOnce(ch *proberChannel) {
	login := ch.login

	if ch.mediaURL == "" || time.Since(ch.mediaFetched) > mediaPlaylistTTL {
		if !p.resolveMediaPlaylist(ch) {
			return
		}
	}

	chunkBody, status, ok := p.fetchText(ch.mediaURL, 64*1024)
	if !ok {
		// Signed media URL expired / stream restarted: drop the cache and
		// re-resolve on the next tick (or right now if it was merely stale).
		ch.mediaURL = ""
		if status == http.StatusNotFound || status == http.StatusForbidden || status == http.StatusUnauthorized {
			if p.resolveMediaPlaylist(ch) {
				chunkBody, _, ok = p.fetchText(ch.mediaURL, 64*1024)
			}
		}
		if !ok {
			p.log("[Prober] %s media playlist HTTP %d", login, status)
			return
		}
	}

	segments := parseSegmentURLs(chunkBody)
	if len(segments) == 0 {
		return
	}

	// HEAD every segment we have not successfully touched yet, oldest first
	// so the sequence Twitch sees matches a real player.
	newHeads, failed := 0, 0
	for _, segURL := range segments {
		key := segmentKey(segURL)
		if _, dup := ch.seen[key]; dup {
			continue
		}
		if newHeads+failed >= maxSegmentHeadsPerTick {
			break
		}
		if p.headSegment(segURL) {
			p.markSeen(ch, key)
			newHeads++
		} else {
			failed++
		}
		select {
		case <-ch.stopCh:
			return
		case <-p.stopCh:
			return
		default:
		}
	}

	// GET (with byte cap) on the newest chunk — Twitch's drop anti-cheat
	// used to count actual bytes downloaded against the user's session, and
	// some campaigns may still. One capped GET per tick keeps that evidence.
	chunkURL := segments[len(segments)-1]
	chunkReq, err := http.NewRequest("GET", chunkURL, nil)
	if err != nil {
		return
	}
	setStreamHeaders(chunkReq)
	chunkResp, err := p.httpClient.Do(chunkReq)
	if err != nil {
		p.log("[Prober] %s chunk GET failed: %v", login, err)
		return
	}
	bytesRead, _ := io.Copy(io.Discard, io.LimitReader(chunkResp.Body, chunkReadLimit))
	chunkResp.Body.Close()
	p.log("[Prober] %s probe ok (segments: %d in playlist, %d new HEAD, %d failed, %d seen; chunk HTTP %d, %d bytes)",
		login, len(segments), newHeads, failed, len(ch.seen), chunkResp.StatusCode, bytesRead)
}

// resolveMediaPlaylist fetches the usher master playlist and caches the
// lowest-quality media playlist URL on the channel. A changed media URL
// means a new broadcast (or new signed session) — the seen-cache is reset
// so the new segment namespace starts clean.
func (p *StreamProber) resolveMediaPlaylist(ch *proberChannel) bool {
	login := ch.login
	tok, err := p.getToken(login)
	if err != nil {
		p.log("[Prober] %s token fetch failed: %v", login, err)
		return false
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
		return false
	}

	mediaURL := pickLowestQualityVariant(body)
	if mediaURL == "" {
		return false
	}
	if mediaURL != ch.mediaURL {
		ch.seen = make(map[string]struct{})
		ch.seenOrder = ch.seenOrder[:0]
	}
	ch.mediaURL = mediaURL
	ch.mediaFetched = time.Now()
	return true
}

// headSegment issues a HEAD for one media segment. Headers only — no audio
// or video body is transferred. Returns true on any 2xx.
func (p *StreamProber) headSegment(segURL string) bool {
	req, err := http.NewRequest("HEAD", segURL, nil)
	if err != nil {
		return false
	}
	setStreamHeaders(req)
	resp, err := p.headClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (p *StreamProber) markSeen(ch *proberChannel, key string) {
	ch.seen[key] = struct{}{}
	ch.seenOrder = append(ch.seenOrder, key)
	for len(ch.seenOrder) > seenSegmentsCap {
		delete(ch.seen, ch.seenOrder[0])
		ch.seenOrder = ch.seenOrder[1:]
	}
}

func setStreamHeaders(req *http.Request) {
	req.Header.Set("User-Agent", ActiveClient().UserAgent)
	req.Header.Set("Origin", "https://www.twitch.tv")
	req.Header.Set("Referer", "https://www.twitch.tv/")
}

func (p *StreamProber) fetchText(u string, limit int64) (string, int, bool) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", 0, false
	}
	setStreamHeaders(req)
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

// parseSegmentURLs returns every media segment URL of an HLS media playlist
// in playlist order. Twitch serves both legacy .ts and modern .mp4 (CMAF)
// chunks depending on the transcode pipeline — match by /segment/ path which
// is consistent across both. Skips .m3u8 lines (next-playlist references);
// #EXT-X-MAP init segments live on tag lines and are skipped implicitly.
func parseSegmentURLs(playlist string) []string {
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

// segmentKey identifies a segment independent of per-request query noise
// (signed URL parameters rotate while the segment path stays the same).
func segmentKey(segURL string) string {
	if i := strings.IndexByte(segURL, '?'); i >= 0 {
		return segURL[:i]
	}
	return segURL
}
