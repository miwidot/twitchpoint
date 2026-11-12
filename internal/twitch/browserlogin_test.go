package twitch

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

// stubBrowser stops the tests from launching a real browser on the machine
// running them. Restored on cleanup so one test cannot leak into another.
func stubBrowser(t *testing.T) {
	t.Helper()
	prev := browserOpener
	browserOpener = func(string) error { return nil }
	t.Cleanup(func() { browserOpener = prev })
}

func TestBrowserLoginURL_CarriesClientRedirectAndScopes(t *testing.T) {
	raw := BrowserLoginURL(ClientAndroid.ClientID)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("not a URL: %v", err)
	}
	if got, want := u.Scheme+"://"+u.Host+u.Path, authorizeURL; got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
	q := u.Query()
	if got := q.Get("client_id"); got != ClientAndroid.ClientID {
		t.Errorf("client_id = %q", got)
	}
	// The trailing slash is not cosmetic — it is what the client has
	// registered, and Twitch rejects the redirect without it.
	if got := q.Get("redirect_uri"); got != "https://www.twitch.tv/" {
		t.Errorf("redirect_uri = %q, want the exact registered value with trailing slash", got)
	}
	if got := q.Get("response_type"); got != "token" {
		t.Errorf("response_type = %q, want token (implicit grant)", got)
	}
	// Dropping the scopes would still log in, but silently cost channel
	// points and chat.
	if got := q.Get("scope"); got != oauthScopes {
		t.Errorf("scope = %q, want %q", got, oauthScopes)
	}
}

func TestExtractAccessToken(t *testing.T) {
	const token = "abcdef0123456789abcdef0123"
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "full redirect URL as copied from the address bar",
			input: "https://www.twitch.tv/#access_token=" + token + "&scope=chat%3Aread&token_type=bearer",
			want:  token,
		},
		{
			name:  "fragment only",
			input: "#access_token=" + token,
			want:  token,
		},
		{
			name:  "token not first in the fragment",
			input: "https://www.twitch.tv/#token_type=bearer&access_token=" + token,
			want:  token,
		},
		{
			name:  "bare token",
			input: token,
			want:  token,
		},
		{
			name:  "surrounding whitespace from the paste",
			input: "  " + token + "\t",
			want:  token,
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
		{
			name:  "unrelated text",
			input: "no token here",
			want:  "",
		},
		{
			name:  "too short to be a token",
			input: "abc123",
			want:  "",
		},
		{
			name:  "the login URL itself, pasted back by mistake",
			input: BrowserLoginURL(ClientAndroid.ClientID),
			want:  "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractAccessToken(c.input); got != c.want {
				t.Errorf("ExtractAccessToken(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestBrowserLogin_AcceptsPastedURL(t *testing.T) {
	stubBrowser(t)
	const token = "zyxwvu9876543210zyxwvu9876"
	in := strings.NewReader("https://www.twitch.tv/#access_token=" + token + "&token_type=bearer\n")
	var out bytes.Buffer

	got, err := BrowserLogin(ClientAndroid.ClientID, in, &out)
	if err != nil {
		t.Fatalf("BrowserLogin: %v", err)
	}
	if got != token {
		t.Fatalf("token = %q, want %q", got, token)
	}
	if !strings.Contains(out.String(), BrowserLoginURL(ClientAndroid.ClientID)) {
		t.Error("the login URL was never shown to the user")
	}
}

// A blank line is the usual "pressed Enter too early" mistake and must not
// abort the login, and a mis-copy must be re-promptable rather than fatal.
func TestBrowserLogin_RepromptsOnBlankAndGarbage(t *testing.T) {
	stubBrowser(t)
	const token = "aaaabbbbccccddddeeeeffff11"
	in := strings.NewReader("\n   \nnot a token\nhttps://www.twitch.tv/#access_token=" + token + "\n")
	var out bytes.Buffer

	got, err := BrowserLogin(ClientAndroid.ClientID, in, &out)
	if err != nil {
		t.Fatalf("BrowserLogin: %v", err)
	}
	if got != token {
		t.Fatalf("token = %q, want %q", got, token)
	}
	if !strings.Contains(out.String(), "Nothing pasted") {
		t.Error("blank input was not reported back to the user")
	}
	if !strings.Contains(out.String(), "does not look like a token") {
		t.Error("garbage input was not reported back to the user")
	}
}

// A closed input (a pipe, a container with no TTY) must fail with advice
// instead of looping forever.
func TestBrowserLogin_EOFFailsWithGuidance(t *testing.T) {
	stubBrowser(t)
	_, err := BrowserLogin(ClientAndroid.ClientID, strings.NewReader(""), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error on closed input")
	}
	if !strings.Contains(err.Error(), "TWITCH_TOKEN") {
		t.Errorf("error should point at the non-interactive route, got: %v", err)
	}
}

func TestClientMismatchWarning(t *testing.T) {
	if w := ClientMismatchWarning(&TokenInfo{ClientID: ClientAndroid.ClientID}, ClientAndroid); w != "" {
		t.Errorf("matching client must not warn, got: %s", w)
	}
	if w := ClientMismatchWarning(nil, ClientAndroid); w != "" {
		t.Errorf("nil info must not warn, got: %s", w)
	}
	w := ClientMismatchWarning(&TokenInfo{ClientID: ClientTV.ClientID}, ClientAndroid)
	if w == "" {
		t.Fatal("a TV-minted token used as the Android client must warn")
	}
	if !strings.Contains(w, "Smart-TV") {
		t.Errorf("warning should name the minting client, got: %s", w)
	}
	if !strings.Contains(w, "NO watch time") {
		t.Errorf("warning should state the consequence, got: %s", w)
	}
}
