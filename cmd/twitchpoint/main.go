package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/farmer"
	"github.com/miwi/twitchpoint/internal/twitch"
	"github.com/miwi/twitchpoint/internal/web"
)

const appVersion = "2.0.0"

func main() {
	web.Version = appVersion
	configPath := flag.String("config", "", "Path to config file (default: config.json)")
	addChannel := flag.String("add-channel", "", "Add a channel to config (validates against Twitch + persists ID) and exit")
	removeChannel := flag.String("remove-channel", "", "Remove a channel from config and exit (use for renamed/deleted channels)")
	setToken := flag.String("token", "", "Set auth token and exit")
	forceLogin := flag.Bool("login", false, "Force re-login via Twitch Device Code OAuth")
	headless := flag.Bool("headless", false, "Run without TUI (for Docker/servers)")
	flag.Parse()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Handle --token flag (manual token override)
	if *setToken != "" {
		cfg.SetAuthToken(*setToken)
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
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
			log.Fatalf("Cannot add channel: no auth token. Run --login first or set --token.")
		}
		gql := twitch.NewGQLClient(token)
		info, err := gql.GetChannelInfo(channel)
		if err != nil {
			log.Fatalf("Channel %q not found on Twitch: %v", channel, err)
		}
		added := cfg.AddChannel(info.Login)
		cfg.SetChannelID(info.Login, info.ID)
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
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
				log.Fatalf("Failed to save config: %v", err)
			}
			fmt.Printf("Removed channel %q from config\n", channel)
		} else {
			fmt.Printf("Channel %q not found in config\n", channel)
		}
		return
	}

	// Handle --login flag (force re-login via Device Code OAuth)
	if *forceLogin {
		token, err := twitch.DeviceCodeLogin(twitch.TVClientID)
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		cfg.SetAuthToken(token)
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Token saved to %s\n", cfg.Path())
		return
	}

	// First-run setup: auto-login via Device Code OAuth if no token
	if cfg.GetAuthToken() == "" {
		fmt.Println("Welcome to TwitchPoint Farmer!")
		fmt.Println()
		token, err := twitch.DeviceCodeLogin(twitch.TVClientID)
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		cfg.SetAuthToken(token)
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Token saved to %s\n", cfg.Path())
		fmt.Println()
	}

	// Start farmer
	f := farmer.New(cfg, appVersion)
	if err := f.Start(); err != nil {
		// Auth failure likely means token was created with old Client-ID — auto re-login
		if strings.Contains(err.Error(), "auth validation failed") {
			fmt.Println("Auth token expired or invalid (Client-ID changed). Re-authenticating...")
			fmt.Println()
			token, err := twitch.DeviceCodeLogin(twitch.TVClientID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Re-login failed: %v\n", err)
				os.Exit(1)
			}
			cfg.SetAuthToken(token)
			if err := cfg.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("New token saved to %s\n", cfg.Path())
			fmt.Println()

			// Retry start with new token
			f = farmer.New(cfg, appVersion)
			if err := f.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start farmer: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Failed to start farmer: %v\n", err)
			os.Exit(1)
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
