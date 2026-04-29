package twitch

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// newChromeTransport returns an http.Transport whose TLS handshake mimics
// Chrome on Windows (HelloChrome_Auto). Twitch's drop-credit anti-cheat
// fingerprints the TLS ClientHello — Go's default net/http handshake gets
// flagged as "non-standard" and silently dropped from credit, while the
// Python aiohttp / browser handshakes pass. uTLS bypasses this by speaking
// a real Chrome handshake.
//
// Used for ALL outbound HTTPS to gql.twitch.tv, usher.ttvnw.net, video CDN.
// Plain net.Conn-based services (PubSub WebSocket, IRC) keep their own
// dialers since they don't go through this transport.
func newChromeTransport() *http.Transport {
	dialTLS := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		rawConn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			rawConn.Close()
			return nil, err
		}
		uconfig := &utls.Config{ServerName: host}
		uconn := utls.UClient(rawConn, uconfig, utls.HelloChrome_Auto)
		if err := uconn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		return uconn, nil
	}
	return &http.Transport{
		DialTLSContext:        dialTLS,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		// uTLS gives us the *Conn but ALPN negotiation can still produce
		// h2; let net/http detect via the connection state.
		TLSNextProto: map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
	}
}
