package twitch

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	ircHost     = "irc.chat.twitch.tv"
	ircPort     = 6697 // TLS
	ircPingFreq = 4 * time.Minute
)

// IRCClient manages a connection to Twitch IRC for viewer presence.
type IRCClient struct {
	token    string
	username string
	logFunc  func(format string, args ...interface{})

	mu       sync.Mutex
	conn     net.Conn
	writer   *bufio.Writer
	channels map[string]bool // login -> joined
	stopCh   chan struct{}
	stopped  bool
}

// NewIRCClient creates a new IRC client.
func NewIRCClient(token, username string, logFunc func(format string, args ...interface{})) *IRCClient {
	return &IRCClient{
		token:    token,
		username: strings.ToLower(username),
		logFunc:  logFunc,
		channels: make(map[string]bool),
		stopCh:   make(chan struct{}),
	}
}

// Connect establishes the IRC connection and authenticates.
func (c *IRCClient) Connect() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	go c.connectLoop()
	return nil
}

func (c *IRCClient) connectLoop() {
	backoff := time.Second

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		if err := c.connect(); err != nil {
			c.log("[IRC] Connection error: %v", err)
			c.log("[IRC] Reconnecting in %v...", backoff)

			select {
			case <-time.After(backoff):
			case <-c.stopCh:
				return
			}

			// Exponential backoff (max 30s)
			backoff = backoff * 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}

		// Connected successfully, reset backoff
		backoff = time.Second

		// Run read loop (blocks until disconnect)
		disconnectReason := c.readLoop()
		c.log("[IRC] Disconnected: %s", disconnectReason)

		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
	}
}

func (c *IRCClient) connect() error {
	// Connect with TLS
	conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", ircHost, ircPort), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	c.mu.Unlock()

	// Authenticate
	if err := c.send("PASS oauth:" + c.token); err != nil {
		return fmt.Errorf("PASS: %w", err)
	}
	if err := c.send("NICK " + c.username); err != nil {
		return fmt.Errorf("NICK: %w", err)
	}

	// Request capabilities for better integration
	if err := c.send("CAP REQ :twitch.tv/membership"); err != nil {
		return fmt.Errorf("CAP: %w", err)
	}

	c.log("[IRC] Connected as %s", c.username)

	// Rejoin all channels
	c.mu.Lock()
	channels := make([]string, 0, len(c.channels))
	for ch := range c.channels {
		channels = append(channels, ch)
	}
	c.mu.Unlock()

	for _, ch := range channels {
		c.joinChannel(ch)
	}

	return nil
}

func (c *IRCClient) readLoop() string {
	reader := bufio.NewReader(c.conn)
	pingTicker := time.NewTicker(ircPingFreq)
	defer pingTicker.Stop()

	// Read in goroutine
	lines := make(chan string)
	errors := make(chan error)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				errors <- err
				return
			}
			lines <- strings.TrimSpace(line)
		}
	}()

	for {
		select {
		case <-c.stopCh:
			return "stopped"

		case err := <-errors:
			return err.Error()

		case line := <-lines:
			c.handleLine(line)

		case <-pingTicker.C:
			// Send our own PING to keep connection alive
			if err := c.send("PING :tmi.twitch.tv"); err != nil {
				return "ping failed"
			}
		}
	}
}

func (c *IRCClient) handleLine(line string) {
	// Handle PING from server
	if strings.HasPrefix(line, "PING") {
		c.send("PONG" + strings.TrimPrefix(line, "PING"))
		return
	}

	// Log auth failures
	if strings.Contains(line, "Login authentication failed") {
		c.log("[IRC] Authentication failed - check token")
	}

	// Log successful joins (optional, for debugging)
	// Format: :username!username@username.tmi.twitch.tv JOIN #channel
	if strings.Contains(line, " JOIN #") {
		// Successfully joined
	}
}

func (c *IRCClient) send(msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.writer == nil {
		return fmt.Errorf("not connected")
	}

	_, err := c.writer.WriteString(msg + "\r\n")
	if err != nil {
		return err
	}
	return c.writer.Flush()
}

// Join adds a channel to the join list and joins if connected.
func (c *IRCClient) Join(login string) {
	login = strings.ToLower(login)

	c.mu.Lock()
	c.channels[login] = true
	connected := c.conn != nil
	c.mu.Unlock()

	if connected {
		c.joinChannel(login)
	}
}

func (c *IRCClient) joinChannel(login string) {
	if err := c.send("JOIN #" + login); err != nil {
		c.log("[IRC] Failed to join #%s: %v", login, err)
	}
}

// Part leaves a channel.
func (c *IRCClient) Part(login string) {
	login = strings.ToLower(login)

	c.mu.Lock()
	delete(c.channels, login)
	connected := c.conn != nil
	c.mu.Unlock()

	if connected {
		if err := c.send("PART #" + login); err != nil {
			c.log("[IRC] Failed to part #%s: %v", login, err)
		}
	}
}

// Close shuts down the IRC client.
func (c *IRCClient) Close() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	close(c.stopCh)

	if c.conn != nil {
		c.send("QUIT :Goodbye")
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	c.log("[IRC] Disconnected")
}

func (c *IRCClient) log(format string, args ...interface{}) {
	if c.logFunc != nil {
		c.logFunc(format, args...)
	}
}
