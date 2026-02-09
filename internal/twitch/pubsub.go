package twitch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pubsubURL       = "wss://pubsub-edge.twitch.tv/v1"
	pingInterval    = 4*time.Minute + 30*time.Second // Twitch expects pings within 5 min
	pongTimeout     = 10 * time.Second
	maxTopics       = 50
	reconnectBase   = 1 * time.Second
	reconnectMax    = 2 * time.Minute
)

// PubSubClient manages a WebSocket connection to Twitch PubSub.
type PubSubClient struct {
	authToken string
	events    chan FarmerEvent

	mu      sync.Mutex
	writeMu sync.Mutex // serializes all WebSocket writes
	conn    *websocket.Conn
	topics  map[string]bool
	closed  bool
	closeCh chan struct{}
}

// NewPubSubClient creates a new PubSub client. Events are delivered on the returned channel.
func NewPubSubClient(authToken string, events chan FarmerEvent) *PubSubClient {
	return &PubSubClient{
		authToken: authToken,
		events:    events,
		topics:    make(map[string]bool),
		closeCh:   make(chan struct{}),
	}
}

// Connect establishes the WebSocket connection with auto-reconnect.
func (p *PubSubClient) Connect() error {
	return p.connectWithRetry()
}

func (p *PubSubClient) connectWithRetry() error {
	backoff := reconnectBase

	for {
		select {
		case <-p.closeCh:
			return nil
		default:
		}

		connectedAt := time.Now()
		err := p.connectOnce()
		if err == nil {
			disconnectReason := p.readLoop()

			// readLoop exited, check if intentionally closed
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return nil
			}
			p.mu.Unlock()

			// Only reset backoff if connection was stable (lasted > 30s)
			if time.Since(connectedAt) > 30*time.Second {
				backoff = reconnectBase
			}

			p.sendError(fmt.Errorf("disconnected (%s), reconnecting in %v", disconnectReason, backoff))
		} else {
			p.sendError(fmt.Errorf("connection failed: %v, retrying in %v", err, backoff))
		}

		select {
		case <-time.After(backoff):
		case <-p.closeCh:
			return nil
		}

		backoff *= 2
		if backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

func (p *PubSubClient) connectOnce() error {
	conn, _, err := websocket.DefaultDialer.Dial(pubsubURL, nil)
	if err != nil {
		return fmt.Errorf("dial pubsub: %w", err)
	}

	p.mu.Lock()
	// Close old connection before replacing
	if p.conn != nil {
		p.conn.Close()
	}
	p.conn = conn
	topics := make([]string, 0, len(p.topics))
	for t := range p.topics {
		topics = append(topics, t)
	}
	p.mu.Unlock()

	// Subscribe in batches to avoid "message too big" (Twitch rejects large LISTEN frames)
	const batchSize = 10
	for i := 0; i < len(topics); i += batchSize {
		end := i + batchSize
		if end > len(topics) {
			end = len(topics)
		}
		if err := p.sendListen(topics[i:end]); err != nil {
			conn.Close()
			return fmt.Errorf("resubscribe batch: %w", err)
		}
	}

	p.sendError(fmt.Errorf("connected, subscribed to %d topics", len(topics)))
	return nil
}

func (p *PubSubClient) readLoop() string {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	// done channel stops the ping goroutine when readLoop exits
	done := make(chan struct{})
	defer close(done)

	// Start ping goroutine
	go func() {
		for {
			select {
			case <-pingTicker.C:
				p.mu.Lock()
				conn := p.conn
				p.mu.Unlock()
				if conn == nil {
					return
				}
				msg := PubSubOutgoing{Type: PubSubTypePing}
				data, _ := json.Marshal(msg)
				if err := p.writeMessage(data); err != nil {
					return
				}
			case <-done:
				return
			case <-p.closeCh:
				return
			}
		}
	}()

	for {
		p.mu.Lock()
		conn := p.conn
		p.mu.Unlock()
		if conn == nil {
			return "connection lost"
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			return err.Error()
		}

		var incoming PubSubIncoming
		if err := json.Unmarshal(message, &incoming); err != nil {
			continue
		}

		switch incoming.Type {
		case PubSubTypePong:
			// Expected response to PING
		case PubSubTypeReconn:
			// Server requests reconnect
			conn.Close()
			return "server requested reconnect"
		case PubSubTypeResponse:
			if incoming.Error != "" {
				p.sendError(fmt.Errorf("listen error: %s", incoming.Error))
			}
		case PubSubTypeMessage:
			if incoming.Data != nil {
				p.handleMessage(incoming.Data)
			}
		}
	}
}

func (p *PubSubClient) handleMessage(data *PubSubMsgData) {
	topic := data.Topic

	switch {
	case strings.HasPrefix(topic, "community-points-user-v1."):
		p.handleCommunityPoints(data.Message)
	case strings.HasPrefix(topic, "video-playback-by-id."):
		channelID := strings.TrimPrefix(topic, "video-playback-by-id.")
		p.handleVideoPlayback(channelID, data.Message)
	case strings.HasPrefix(topic, "raid."):
		channelID := strings.TrimPrefix(topic, "raid.")
		p.handleRaid(channelID, data.Message)
	}
}

func (p *PubSubClient) handleCommunityPoints(rawMessage string) {
	var evt CommunityPointsEvent
	if err := json.Unmarshal([]byte(rawMessage), &evt); err != nil {
		return
	}

	// Resolve channel ID from multiple possible locations
	channelID := evt.Data.ChannelID

	switch evt.Type {
	case "claim-available":
		if evt.Data.Claim != nil {
			if channelID == "" {
				channelID = evt.Data.Claim.ChannelID
			}
			p.events <- FarmerEvent{
				Type:      EventClaimAvailable,
				ChannelID: channelID,
				Data: ClaimData{
					ClaimID: evt.Data.Claim.ID,
				},
			}
		}
	case "points-earned":
		if evt.Data.PointGain != nil {
			if channelID == "" {
				channelID = evt.Data.PointGain.ChannelID
			}
			totalPoints := 0
			if evt.Data.Balance != nil {
				totalPoints = evt.Data.Balance.Balance
			}
			// Use TotalPoints from point_gain if BaselineGain is 0
			pointsGained := evt.Data.PointGain.BaselineGain
			if pointsGained == 0 {
				pointsGained = evt.Data.PointGain.TotalPoints
			}
			p.events <- FarmerEvent{
				Type:      EventPointsEarned,
				ChannelID: channelID,
				Data: PointsData{
					PointsGained: pointsGained,
					TotalPoints:  totalPoints,
					ReasonCode:   evt.Data.PointGain.ReasonCode,
				},
			}
		}
	case "claim-claimed":
		// Claim was successfully claimed - handled via points-earned
	}
}

func (p *PubSubClient) handleVideoPlayback(channelID, rawMessage string) {
	var evt VideoPlaybackEvent
	if err := json.Unmarshal([]byte(rawMessage), &evt); err != nil {
		return
	}

	switch evt.Type {
	case "stream-up":
		p.events <- FarmerEvent{
			Type:      EventStreamUp,
			ChannelID: channelID,
		}
	case "stream-down":
		p.events <- FarmerEvent{
			Type:      EventStreamDown,
			ChannelID: channelID,
		}
	case "viewcount":
		p.events <- FarmerEvent{
			Type:      EventViewCount,
			ChannelID: channelID,
			Data: ViewCountData{
				Viewers: evt.Viewers,
			},
		}
	}
}

func (p *PubSubClient) handleRaid(channelID, rawMessage string) {
	var evt RaidEvent
	if err := json.Unmarshal([]byte(rawMessage), &evt); err != nil {
		return
	}

	if evt.Raid.ID != "" {
		p.events <- FarmerEvent{
			Type:      EventRaid,
			ChannelID: channelID,
			Data: RaidData{
				RaidID:            evt.Raid.ID,
				TargetLogin:       evt.Raid.TargetLogin,
				TargetDisplayName: evt.Raid.TargetDisplayName,
			},
		}
	}
}

func (p *PubSubClient) sendListen(topics []string) error {
	nonce := generateNonce()
	msg := PubSubOutgoing{
		Type:  PubSubTypeListen,
		Nonce: nonce,
		Data: &PubSubListen{
			Topics:    topics,
			AuthToken: p.authToken,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return p.writeMessage(data)
}

// Listen subscribes to the given PubSub topics.
func (p *PubSubClient) Listen(topics []string) error {
	p.mu.Lock()
	for _, t := range topics {
		p.topics[t] = true
	}
	conn := p.conn
	p.mu.Unlock()

	if conn == nil {
		return nil // Will subscribe on connect
	}

	return p.sendListen(topics)
}

// Unlisten unsubscribes from the given topics.
func (p *PubSubClient) Unlisten(topics []string) error {
	p.mu.Lock()
	for _, t := range topics {
		delete(p.topics, t)
	}
	p.mu.Unlock()

	nonce := generateNonce()
	msg := PubSubOutgoing{
		Type:  PubSubTypeUnlisten,
		Nonce: nonce,
		Data: &PubSubListen{
			Topics: topics,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return p.writeMessage(data)
}

// Close shuts down the PubSub client.
func (p *PubSubClient) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	close(p.closeCh)

	if p.conn != nil {
		p.conn.Close()
	}
}

func (p *PubSubClient) writeMessage(data []byte) error {
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (p *PubSubClient) sendError(err error) {
	select {
	case p.events <- FarmerEvent{Type: EventError, Data: err}:
	default:
	}
}

func generateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
