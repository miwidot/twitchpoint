package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/farmer"
	"github.com/miwi/twitchpoint/internal/twitch"
	"github.com/miwi/twitchpoint/internal/ui"
	"github.com/miwi/twitchpoint/internal/web"
)

const appVersion = "1.2.0-beta.5"

func main() {
	web.Version = appVersion
	configPath := flag.String("config", "", "Path to config file (default: config.json)")
	addChannel := flag.String("add-channel", "", "Add a channel to config and exit")
	setToken := flag.String("token", "", "Set auth token and exit")
	forceLogin := flag.Bool("login", false, "Force re-login via Twitch Device Code OAuth")
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

	// Prompt for channels if none configured
	if len(cfg.GetChannelLogins()) == 0 {
		fmt.Println("No channels configured. Enter channel names (one per line, empty line to finish):")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(strings.ToLower(scanner.Text()))
			if line == "" {
				break
			}
			cfg.AddChannel(line)
			fmt.Printf("  Added: %s\n", line)
		}
		if len(cfg.GetChannelLogins()) > 0 {
			if err := cfg.Save(); err != nil {
				log.Fatalf("Failed to save config: %v", err)
			}
		}
		fmt.Println()
	}

	// Start farmer
	f := farmer.New(cfg)
	if err := f.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start farmer: %v\n", err)
		os.Exit(1)
	}
	defer f.Stop()

	// Start web server if enabled
	if cfg.WebEnabled {
		port := cfg.WebPort
		if port <= 0 {
			port = 8080
		}
		webServer := web.New(f, port)
		go func() {
			fmt.Printf("Web UI available at http://localhost:%d\n", port)
			if err := webServer.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Web server error: %v\n", err)
			}
		}()
	}

	// Silence Go's default logger before TUI starts - all logs go through farmer's internal log
	log.SetOutput(io.Discard)

	// Run TUI
	if err := ui.Run(f); err != nil {
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
		os.Exit(1)
	}
}
