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

const appVersion = "1.3.5"

func main() {
	web.Version = appVersion
	configPath := flag.String("config", "", "Path to config file (default: config.json)")
	addChannel := flag.String("add-channel", "", "Add a channel to config and exit")
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
		cfg.AuthToken = *setToken
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Auth token saved to %s\n", cfg.Path())
		return
	}

	// Handle --add-channel flag
	if *addChannel != "" {
		channel := strings.ToLower(*addChannel)
		if cfg.AddChannel(channel) {
			if err := cfg.Save(); err != nil {
				log.Fatalf("Failed to save config: %v", err)
			}
			fmt.Printf("Added channel %q to config\n", channel)
		} else {
			fmt.Printf("Channel %q already in config\n", channel)
		}
		return
	}

	// Handle --login flag (force re-login via Device Code OAuth)
	if *forceLogin {
		token, err := twitch.DeviceCodeLogin(twitch.TVClientID)
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		cfg.AuthToken = token
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Token saved to %s\n", cfg.Path())
		return
	}

	// First-run setup: auto-login via Device Code OAuth if no token
	if cfg.AuthToken == "" {
		fmt.Println("Welcome to TwitchPoint Farmer!")
		fmt.Println()
		token, err := twitch.DeviceCodeLogin(twitch.TVClientID)
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		cfg.AuthToken = token
		if err := cfg.Save(); err != nil {
			log.Fatalf("Failed to save config: %v", err)
		}
		fmt.Printf("Token saved to %s\n", cfg.Path())
		fmt.Println()
	}

	// Start farmer
	f := farmer.New(cfg, appVersion)
	if err := f.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start farmer: %v\n", err)
		os.Exit(1)
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
	port := cfg.WebPort
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
	fmt.Printf("Web UI: http://localhost:%d\n", port)
	fmt.Println("Press Ctrl+C to stop.")

	// Block until SIGINT or SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\nShutting down...")
}
