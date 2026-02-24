//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/energye/systray"
	"github.com/miwi/twitchpoint/internal/config"
	"github.com/miwi/twitchpoint/internal/farmer"
	"github.com/miwi/twitchpoint/internal/ui"
	"github.com/miwi/twitchpoint/internal/web"
)

//go:embed icon.ico
var trayIcon []byte

func runUI(f *farmer.Farmer, cfg *config.Config) {
	// Intercept console X button — hide instead of terminate
	setupConsoleCloseHandler()

	webPort := cfg.WebPort
	if webPort <= 0 {
		webPort = 8080
	}

	// Start web server if enabled
	if cfg.WebEnabled {
		webServer := web.New(f, webPort)
		go func() {
			fmt.Printf("Web UI available at http://localhost:%d\n", webPort)
			if err := webServer.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Web server error: %v\n", err)
			}
		}()
	}

	// Start system tray in background goroutine
	go startTray(f, cfg, webPort)

	// Silence Go's default logger before TUI starts
	log.SetOutput(io.Discard)

	// Run bubbletea TUI — 'q' hides console instead of quitting.
	// TUI stays running (hidden), tray keeps the app alive.
	// Only "Quit" from tray actually exits (via os.Exit).
	m := ui.NewModel(f)
	m.OnQuit = hideConsole
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "UI error: %v\n", err)
		os.Exit(1)
	}
}

func startTray(f *farmer.Farmer, cfg *config.Config, webPort int) {
	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("TwitchPoint")
		systray.SetTooltip("TwitchPoint Farmer")

		// Show menu on left-click
		systray.SetOnClick(func(menu systray.IMenu) {
			if menu != nil {
				menu.ShowMenu()
			}
		})

		// Header
		mTitle := systray.AddMenuItem("TwitchPoint Farmer v"+appVersion, "")
		mTitle.Disable()

		systray.AddSeparator()

		// Stats (updated periodically)
		mPoints := systray.AddMenuItem("Points: ...", "")
		mPoints.Disable()
		mChannels := systray.AddMenuItem("Channels: ...", "")
		mChannels.Disable()

		systray.AddSeparator()

		// Open Web UI
		if cfg.WebEnabled {
			mWebUI := systray.AddMenuItem("Open Web UI", "Open web dashboard in browser")
			mWebUI.Click(func() {
				openBrowser(fmt.Sprintf("http://localhost:%d", webPort))
			})
		}

		// Console toggle — console is visible on startup (TUI runs in it)
		mConsole := systray.AddMenuItem("Hide Console", "Hide or show the TUI console")
		mConsole.Click(func() {
			if isConsoleVisible() {
				hideConsole()
				mConsole.SetTitle("Show Console")
			} else {
				showConsoleWindow()
				mConsole.SetTitle("Hide Console")
			}
		})

		systray.AddSeparator()

		// Auto-start toggle
		mAutoStart := systray.AddMenuItemCheckbox("Start with Windows", "Auto-start on login", isAutoStartEnabled())
		mAutoStart.Click(func() {
			enabled, err := toggleAutoStart()
			if err == nil {
				if enabled {
					mAutoStart.Check()
				} else {
					mAutoStart.Uncheck()
				}
			}
		})

		systray.AddSeparator()

		// Quit
		mQuit := systray.AddMenuItem("Quit", "Stop farming and exit")
		mQuit.Click(func() {
			f.Stop()
			systray.Quit()
			os.Exit(0)
		})

		// Periodic stats update
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()

			updateStats := func() {
				stats := f.GetStats()
				drops := f.GetActiveDrops()

				pointsText := fmt.Sprintf("Points: %s  |  Claims: %d",
					formatNumber(stats.TotalPointsEarned), stats.TotalClaimsMade)
				channelsText := fmt.Sprintf("Channels: %d/%d/%d  |  Drops: %d",
					stats.ChannelsWatching, stats.ChannelsOnline, stats.ChannelsTotal, len(drops))

				mPoints.SetTitle(pointsText)
				mChannels.SetTitle(channelsText)

				systray.SetTooltip(fmt.Sprintf("TwitchPoint - %s pts, %d channels",
					formatNumber(stats.TotalPointsEarned), stats.ChannelsWatching))
			}

			time.Sleep(2 * time.Second)
			updateStats()

			for {
				select {
				case <-ticker.C:
					updateStats()
				case <-f.Done():
					return
				}
			}
		}()
	}

	onExit := func() {}
	systray.Run(onReady, onExit)
}

func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}
