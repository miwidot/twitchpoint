package twitch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// validateURL reports what a token actually is. Critically it returns the
// client_id the token was MINTED BY, which no GQL call reveals.
const validateURL = "https://id.twitch.tv/oauth2/validate"

// TokenInfo is Twitch's answer from /oauth2/validate.
type TokenInfo struct {
	ClientID  string   `json:"client_id"`
	Login     string   `json:"login"`
	UserID    string   `json:"user_id"`
	Scopes    []string `json:"scopes"`
	ExpiresIn int      `json:"expires_in"` // 0 means it does not expire
}

// ValidateToken asks Twitch what the given token is. A non-200 means the
// token is dead (revoked, or invalidated by a password change).
func ValidateToken(token string) (*TokenInfo, error) {
	req, err := http.NewRequest(http.MethodGet, validateURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build validate request: %w", err)
	}
	req.Header.Set("Authorization", "OAuth "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", validateURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read validate response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token rejected by Twitch (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var info TokenInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse validate response: %w", err)
	}
	if info.ClientID == "" {
		return nil, fmt.Errorf("validate response has no client_id: %s", string(body))
	}
	return &info, nil
}

// ClientMismatchWarning checks a token against the identity we are about to
// present and returns a human-readable warning, or "" when they agree.
//
// This is worth checking on every start, because the failure it catches is
// invisible otherwise: Twitch ACCEPTS a token whose minting client differs
// from the Client-Id header we send, serves campaigns and inventory
// normally, and simply never credits watch time. Without this check the
// symptom is "it runs all night and collects nothing" with no error
// anywhere — which is exactly what Smart-TV tokens produced for the people
// who tried that route.
func ClientMismatchWarning(info *TokenInfo, active ClientType) string {
	if info == nil || info.ClientID == active.ClientID {
		return ""
	}
	name := "an unknown client"
	switch info.ClientID {
	case ClientAndroid.ClientID:
		name = "the Android client"
	case ClientTV.ClientID:
		name = "the Smart-TV client"
	}
	return fmt.Sprintf(
		"token/client mismatch: this token was minted by %s (%s) but requests go out as %q (%s).\n"+
			"Twitch accepts this and then credits NO watch time. Log in again with --login, "+
			"or set tv_client_mode to match the token.",
		name, info.ClientID, active.Name, active.ClientID,
	)
}
