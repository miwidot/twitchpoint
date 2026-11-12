package twitch

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// Browser login (OAuth implicit grant).
//
// WHY THIS EXISTS: Twitch disabled the device-code grant for the Android
// client in September 2026 — POST /oauth2/device answers
// `{"status":400,"message":"invalid client"}` and has kept doing so. The
// implicit grant for the SAME client ID still works, so that is the way back
// in. Re-verified on 2026-11-13: device code 400, implicit grant 302.
//
// The login page itself is bot-protected, so the authorization cannot be
// scripted — it has to happen in a real browser. It does NOT have to be a
// browser WE drive, though: the user's own browser is a real browser, and
// usually already signed in to Twitch. So we print the URL, the user opens
// it, and Twitch parks the token in the address-bar fragment for them to
// copy back. No browser automation, no extra dependency, and it works the
// same on a desktop, a headless server (open the URL anywhere) and in
// Docker.
//
// The token this produces is minted by the Android client ID, which is what
// makes it useful: it pairs with the Client-Id and User-Agent we already
// send, so watch time credits normally. A token minted by a different
// client is accepted by Twitch and then silently earns nothing — see
// ClientType. ValidateToken is used after login to catch exactly that.
const (
	// authorizeURL is Twitch's OAuth entry point. It redirects to the login
	// page when the browser has no Twitch session, and straight back to
	// redirectURI with the token when it does.
	authorizeURL = "https://id.twitch.tv/oauth2/authorize"

	// redirectURI must be EXACTLY this, trailing slash included — it is what
	// the Android client has registered. Anything else fails after the user
	// has already signed in, which is the most annoying possible moment.
	redirectURI = "https://www.twitch.tv/"
)

// accessTokenPattern matches the token in the fragment Twitch redirects to
// (`https://www.twitch.tv/#access_token=XXXX&scope=...&token_type=bearer`).
// Anchored on the parameter name so a token sitting next to other fragment
// parameters is still found, in any order.
var accessTokenPattern = regexp.MustCompile(`access_token=([A-Za-z0-9]+)`)

// bareTokenPattern matches a token pasted on its own, with no URL around it.
// Twitch OAuth tokens are 30 characters of lowercase alphanumerics; the
// bound is deliberately loose (20+) so a format change does not reject a
// perfectly good token, while still refusing short typos and stray words.
var bareTokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{20,}$`)

// BrowserLoginURL builds the authorization URL the user has to open.
//
// The scope list is the same one the device-code flow requested, so a token
// obtained this way keeps channel points and chat working, not just drops.
// Twitch validates scopes at this endpoint (a bogus scope answers HTTP 400),
// and this list is accepted — verified 2026-11-13.
func BrowserLoginURL(clientID string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"token"},
		"scope":         {oauthScopes},
	}
	return authorizeURL + "?" + q.Encode()
}

// ExtractAccessToken pulls the token out of whatever the user pasted. It
// accepts the whole redirected URL (what you get from selecting the address
// bar — by far the easiest thing to copy), just the fragment, or the bare
// token. Returns "" when the input holds no token.
func ExtractAccessToken(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if m := accessTokenPattern.FindStringSubmatch(input); m != nil {
		return m[1]
	}
	if bareTokenPattern.MatchString(input) {
		return input
	}
	return ""
}

// BrowserLogin runs the interactive implicit-grant login against the given
// reader (os.Stdin in production) and writer.
//
// Blank input is re-prompted rather than treated as failure: the most common
// mistake is pressing Enter before the browser has finished redirecting, and
// aborting there would send the user through the whole flow again. Input
// that holds no recognisable token is reported and re-prompted too, so a
// mis-copy is self-correcting. EOF ends the loop with an error, which is
// what a non-interactive run (a pipe, a container without a TTY) produces —
// those should use the TWITCH_TOKEN environment variable instead.
func BrowserLogin(clientID string, in io.Reader, out io.Writer) (string, error) {
	loginURL := BrowserLoginURL(clientID)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "=== Twitch Login ===")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "1. Open this URL in your normal web browser:")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "   %s\n", loginURL)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "2. Sign in to Twitch if it asks. If you are already signed in,")
	fmt.Fprintln(out, "   it goes through on its own.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "3. You end up on a twitch.tv page whose address looks like:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "      https://www.twitch.tv/#access_token=abcdef...&scope=...")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "4. Copy that whole address and paste it here, then press Enter.")
	fmt.Fprintln(out)

	if err := browserOpener(loginURL); err != nil {
		fmt.Fprintf(out, "(Could not open the browser automatically: %v)\n", err)
		fmt.Fprintln(out, "(Open the URL above by hand — the rest of the steps are the same.)")
		fmt.Fprintln(out)
	}

	scanner := bufio.NewScanner(in)
	// The pasted URL can be long; the default 64 KiB token limit is plenty,
	// but set it explicitly so a future default change cannot truncate it.
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)

	for {
		fmt.Fprint(out, "Paste the address (or just the token): ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("reading pasted token: %w", err)
			}
			return "", fmt.Errorf("no token entered (input closed) — " +
				"for a non-interactive run set the TWITCH_TOKEN environment variable instead")
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			fmt.Fprintln(out, "Nothing pasted — waiting. Take your time; the browser has to finish redirecting first.")
			continue
		}
		if token := ExtractAccessToken(line); token != "" {
			return token, nil
		}
		fmt.Fprintln(out, "That does not look like a token or a Twitch redirect URL.")
		fmt.Fprintln(out, "Expected something containing 'access_token=' — please try again.")
	}
}

// browserOpener is indirected through a variable so tests can run the login
// flow without actually launching a browser on the machine running them.
var browserOpener = openInBrowser

// openInBrowser tries to open the URL in the user's default browser. It is a
// convenience only: every caller prints the URL first, so a failure here is
// reported and the flow continues by hand. A headless box has no browser to
// open and is expected to fail.
func openInBrowser(target string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		// rundll32 avoids the quoting trap of `cmd /c start`, where the URL's
		// & separators would be parsed as command separators.
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	args = append(args, target)
	if _, err := exec.LookPath(cmd); err != nil {
		return fmt.Errorf("%s not found", cmd)
	}
	return exec.Command(cmd, args...).Start()
}
