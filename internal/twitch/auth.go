package twitch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	deviceCodeURL = "https://id.twitch.tv/oauth2/device"
	tokenURL      = "https://id.twitch.tv/oauth2/token"

	// Scopes needed for community points, chat, and raids
	oauthScopes = "channel:read:redemptions user:read:email chat:read chat:edit user:write:chat"
)

// DeviceCodeResponse is the response from the device code request.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// TokenResponse is the response from the token polling endpoint.
type TokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int      `json:"expires_in"`
	TokenType    string   `json:"token_type"`
	Scope        []string `json:"scope"`
}

// tokenErrorResponse is the error response during token polling.
type tokenErrorResponse struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// DeviceCodeLogin orchestrates the full TV Device Code OAuth flow.
// It requests a device code, prints instructions for the user, and polls
// until the user authorizes or the code expires.
func DeviceCodeLogin(clientID string) (string, error) {
	dcr, err := requestDeviceCode(clientID)
	if err != nil {
		return "", fmt.Errorf("request device code: %w", err)
	}

	fmt.Println()
	fmt.Println("=== Twitch Login ===")
	fmt.Printf("1. Open: %s\n", dcr.VerificationURI)
	fmt.Printf("2. Enter code: %s\n", dcr.UserCode)
	fmt.Println("3. Authorize the application")
	fmt.Println()
	fmt.Println("Waiting for authorization...")

	token, err := pollForToken(clientID, dcr.DeviceCode, dcr.Interval, dcr.ExpiresIn)
	if err != nil {
		return "", fmt.Errorf("poll for token: %w", err)
	}

	fmt.Println("Login successful!")
	return token, nil
}

// requestDeviceCode sends a POST to Twitch's device code endpoint.
func requestDeviceCode(clientID string) (*DeviceCodeResponse, error) {
	form := url.Values{
		"client_id": {clientID},
		"scopes":    {oauthScopes},
	}

	resp, err := http.Post(deviceCodeURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", deviceCodeURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var dcr DeviceCodeResponse
	if err := json.Unmarshal(body, &dcr); err != nil {
		return nil, fmt.Errorf("parse device code response: %w", err)
	}

	if dcr.DeviceCode == "" || dcr.UserCode == "" {
		return nil, fmt.Errorf("empty device_code or user_code in response: %s", string(body))
	}

	return &dcr, nil
}

// pollForToken polls Twitch's token endpoint until the user authorizes,
// the code expires, or authorization is denied.
func pollForToken(clientID, deviceCode string, interval, expiresIn int) (string, error) {
	if interval < 1 {
		interval = 5
	}

	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for {
		<-ticker.C

		if time.Now().After(deadline) {
			return "", fmt.Errorf("device code expired — please try again")
		}

		form := url.Values{
			"client_id":   {clientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}

		resp, err := http.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if err != nil {
			continue // transient network error, retry
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var tr TokenResponse
			if err := json.Unmarshal(body, &tr); err != nil {
				return "", fmt.Errorf("parse token response: %w", err)
			}
			if tr.AccessToken == "" {
				return "", fmt.Errorf("empty access_token in response")
			}
			return tr.AccessToken, nil
		}

		// Check error type — authorization_pending means keep polling
		var errResp tokenErrorResponse
		if err := json.Unmarshal(body, &errResp); err == nil {
			msg := strings.ToLower(errResp.Message)
			if strings.Contains(msg, "authorization_pending") {
				continue
			}
			if strings.Contains(msg, "slow_down") {
				// Back off: double the interval
				ticker.Reset(time.Duration(interval*2) * time.Second)
				continue
			}
			// access_denied, expired_token, or other terminal error
			return "", fmt.Errorf("authorization failed: %s", errResp.Message)
		}
	}
}
