//go:build !windows

package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/farmer"
	"github.com/miwi/twitchpoint/internal/ui"
	"github.com/miwi/twitchpoint/internal/web"
)

func runUI(f *farmer.Farmer, cfg *config.Config) {
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

	// Silence Go's default logger before TUI starts
	log.SetOutput(io.Discard)

	// Run TUI (blocking)
	if err := ui.Run(f); err != nil {
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
		os.Exit(1)
	}
}
