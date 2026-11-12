package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/farmer"
	"github.com/miwi/twitchpoint/internal/twitch"
	"github.com/miwi/twitchpoint/internal/web"
)

const appVersion = "2.6.0-beta.1"

func main() {
	web.Version = appVersion
	configPath := flag.String("config", "", "Path to config file (default: config.json)")
	addChannel := flag.String("add-channel", "", "Add a channel to config (validates against Twitch + persists ID) and exit")
	removeChannel := flag.String("remove-channel", "", "Remove a channel from config and exit (use for renamed/deleted channels)")
	setToken := flag.String("token", "", "Set auth token and exit")
	forceLogin := flag.Bool("login", false, "Force re-login via Twitch OAuth in your browser")
	headless := flag.Bool("headless", false, "Run without TUI (for Docker/servers)")
	flag.Parse()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatalf("Failed to load config: %v", err)
	}
	log.Printf("Using config file: %s", cfg.Path())

	// Select the Twitch identity BEFORE any request, including the login
	// flow. Client-Id and User-Agent must match the client that minted the
	// token — a mismatch is accepted by Twitch but silently earns no watch
	// time. See internal/twitch/clienttype.go.
	if cfg.GetTVClientMode() {
		twitch.SetActiveClient(twitch.ClientTV)
		fmt.Println("WARNING: tv_client_mode is DEPRECATED and does not work.")
		fmt.Println("Smart-TV tokens log in, but Twitch returns an empty campaign list for")
		fmt.Println("them and credits no watch time — it farms nothing. It only existed")
		fmt.Println("because the Android device-code login had broken; --login now uses the")
		fmt.Println("browser OAuth flow instead, which mints a working Android token.")
		fmt.Println("Set tv_client_mode to false and run with --login.")
		fmt.Println()
	}

	// TWITCH_TOKEN wins over the stored token. This is the supported way to
	// run headless or in a container: the browser login cannot happen inside
	// a container, so the user logs in anywhere and passes the token in. It
	// is deliberately NOT written to the config — an env-supplied secret
	// should stay in the environment.
	if envToken := os.Getenv("TWITCH_TOKEN"); envToken != "" {
		cfg.SetAuthToken(envToken)
		fmt.Println("Using the auth token from the TWITCH_TOKEN environment variable (not saved to config).")
	}

	// Handle --token flag (manual token override)
	if *setToken != "" {
		cfg.SetAuthToken(*setToken)
		if err := cfg.Save(); err != nil {
			fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Auth token saved to %s\n", cfg.Path())
		return
	}

	// Handle --add-channel flag — validates the channel exists on Twitch
	// and persists BOTH login and ID. Storing the ID is critical: it
	// makes future startups robust against the streamer renaming or
	// briefly unpublishing the channel (rename-detection in
	// addChannelFromEntry only works when the ID is known).
	if *addChannel != "" {
		channel := strings.ToLower(*addChannel)
		token := cfg.GetAuthToken()
		if token == "" {
			fatalf("Cannot add channel: no auth token. Run --login first or set --token.")
		}
		gql := twitch.NewGQLClient(token)
		info, err := gql.GetChannelInfo(channel)
		if err != nil {
			fatalf("Channel %q not found on Twitch: %v", channel, err)
		}
		added := cfg.AddChannel(info.Login)
		cfg.SetChannelID(info.Login, info.ID)
		if err := cfg.Save(); err != nil {
			fatalf("Failed to save config: %v", err)
		}
		if added {
			fmt.Printf("Added channel %s (id=%s) to config\n", info.Login, info.ID)
		} else {
			fmt.Printf("Channel %s already in config — ID refreshed to %s\n", info.Login, info.ID)
		}
		return
	}

	// Handle --remove-channel flag — drops a channel from config. Useful
	// for cleaning up legacy entries (added before ID-tracking, where
	// the streamer has since renamed/deleted) that fail to resolve at
	// startup. Matches the case-insensitive login lookup the registry
	// uses; takes effect on next start.
	if *removeChannel != "" {
		channel := strings.ToLower(*removeChannel)
		if cfg.RemoveChannel(channel) {
			if err := cfg.Save(); err != nil {
				fatalf("Failed to save config: %v", err)
			}
			fmt.Printf("Removed channel %q from config\n", channel)
		} else {
			fmt.Printf("Channel %q not found in config\n", channel)
		}
		return
	}

	// Handle --login flag (force re-login via Device Code OAuth)
	if *forceLogin {
		token, err := twitch.BrowserLogin(twitch.ActiveClient().ClientID, os.Stdin, os.Stdout)
		if err != nil {
			fatalf("Login failed: %v", err)
		}
		reportTokenIdentity(token)
		cfg.SetAuthToken(token)
		if err := cfg.Save(); err != nil {
			fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Token saved to %s\n", cfg.Path())
		return
	}

	// First-run setup: auto-login via Device Code OAuth if no token
	if cfg.GetAuthToken() == "" {
		fmt.Println("Welcome to TwitchPoint Farmer!")
		fmt.Println()
		if cfg.WasCreated() {
			fmt.Println("No existing config found - creating a new login.")
			fmt.Printf("A new config was created at: %s\n", cfg.Path())
		} else {
			fmt.Printf("Config found at %s but it has no login token - starting a fresh login.\n", cfg.Path())
		}
		if runtime.GOOS == "windows" {
			fmt.Println("(Config is searched in: the exe's folder, the current folder, then %APPDATA%\\twitchpoint)")
		}
		fmt.Println()
		token, err := twitch.BrowserLogin(twitch.ActiveClient().ClientID, os.Stdin, os.Stdout)
		if err != nil {
			fatalf("Login failed: %v", err)
		}
		reportTokenIdentity(token)
		cfg.SetAuthToken(token)
		if err := cfg.Save(); err != nil {
			fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Token saved to %s\n", cfg.Path())
		fmt.Println()
	}

	// Check the stored token's identity before starting. This matters most
	// for a token that predates this build: anyone who logged in via the
	// Smart-TV route has a token that is accepted everywhere and credits
	// nothing, and without this line there is no hint why the miner farms
	// all night for zero drops (issue #21). One HTTP call, once per start.
	warnOnTokenMismatch(cfg.GetAuthToken())

	// Start farmer
	f := farmer.New(cfg, appVersion)
	if err := f.Start(); err != nil {
		// Auth failure likely means token was created with old Client-ID — auto re-login
		if strings.Contains(err.Error(), "auth validation failed") {
			fmt.Println("Auth token expired or invalid (Client-ID changed). Re-authenticating...")
			fmt.Println()
			token, err := twitch.BrowserLogin(twitch.ActiveClient().ClientID, os.Stdin, os.Stdout)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Re-login failed: %v\n", err)
				exitWithPause(1)
			}
			reportTokenIdentity(token)
			cfg.SetAuthToken(token)
			if err := cfg.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
				exitWithPause(1)
			}
			fmt.Printf("New token saved to %s\n", cfg.Path())
			fmt.Println()

			// Retry start with new token
			f = farmer.New(cfg, appVersion)
			if err := f.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start farmer: %v\n", err)
				exitWithPause(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Failed to start farmer: %v\n", err)
			exitWithPause(1)
		}
	}
	defer f.Stop()

	// Headless mode: no TUI, just farmer + web server + wait for signal
	if *headless {
		runHeadless(f, cfg)
		return
	}

	// Platform-specific UI (defined in ui_default.go / ui_windows.go)
	runUI(f, cfg)
}

func runHeadless(f *farmer.Farmer, cfg *config.Config) {
	// Start web server (force-enable in headless mode)
	port := cfg.GetWebPort()
	if port <= 0 {
		port = 8080
	}
	webServer := web.New(f, port)
	go func() {
		if err := webServer.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Web server error: %v\n", err)
		}
	}()

	fmt.Printf("TwitchPoint Farmer v%s (headless)\n", appVersion)
	fmt.Printf("Web UI: http://%s\n", webServer.Addr())
	fmt.Println("Press Ctrl+C to stop.")

	// Block until SIGINT or SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\nShutting down...")
}

// fatalf prints the error and exits. On Windows it first waits for Enter so a
// double-clicked exe does not close the console before the message is read.
func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	exitWithPause(1)
}

// exitWithPause exits with the given code. On Windows it first waits for
// Enter so a double-clicked exe does not close its console before the error
// can be read — but ONLY when stdin is an interactive console. Unattended
// runs (a Windows service, a scheduled task, a redirected or absent stdin)
// would otherwise block here forever instead of exiting, and no restart
// mechanism could take over. See shouldPauseOnExit.
func exitWithPause(code int) {
	if shouldPauseOnExit() {
		fmt.Fprintln(os.Stderr, "\nPress Enter to close...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

// reportTokenIdentity tells the user what the token they just obtained
// actually is, and warns when its minting client does not match the identity
// we send on every request.
//
// This check exists because that mismatch has no other symptom: Twitch
// accepts the token, serves campaigns and inventory, and simply never
// credits watch time. The miner then runs all night and collects nothing,
// with nothing in the logs to explain it. A validation failure here is only
// reported, never fatal — the token may still be fine and the farmer's own
// startup check is the authoritative one.
func reportTokenIdentity(token string) {
	info, err := twitch.ValidateToken(token)
	if err != nil {
		fmt.Printf("Note: could not verify the new token with Twitch: %v\n", err)
		return
	}
	fmt.Printf("Logged in as %s (user ID %s).\n", info.Login, info.UserID)
	if warning := twitch.ClientMismatchWarning(info, twitch.ActiveClient()); warning != "" {
		fmt.Println()
		fmt.Println("WARNING: " + warning)
		fmt.Println()
	}
}

// warnOnTokenMismatch reports a stored token whose minting client does not
// match the identity we send, and stays silent otherwise.
//
// Deliberately non-fatal and quiet on error: a network blip at startup must
// not stop the miner, and the farmer's own GetUserInfo check right after
// this is the authoritative test of whether the token works at all. This
// only answers the question that check cannot — WHICH client the token
// belongs to.
func warnOnTokenMismatch(token string) {
	if token == "" {
		return
	}
	info, err := twitch.ValidateToken(token)
	if err != nil {
		return // farmer.Start reports a genuinely dead token with context
	}
	if warning := twitch.ClientMismatchWarning(info, twitch.ActiveClient()); warning != "" {
		fmt.Println()
		fmt.Println("WARNING: " + warning)
		fmt.Println()
	}
}
