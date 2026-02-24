package farmer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UpdateInfo holds the public state of the update checker.
type UpdateInfo struct {
	HasStableUpdate bool   `json:"has_stable_update"`
	HasBetaUpdate   bool   `json:"has_beta_update"`
	LatestStable    string `json:"latest_stable"`
	LatestBeta      string `json:"latest_beta"`
	StableURL       string `json:"stable_url"`
	BetaURL         string `json:"beta_url"`
	CurrentVersion  string `json:"current_version"`
	IsBeta          bool   `json:"is_beta"`
}

// updateState holds internal mutable state for the update checker.
type updateState struct {
	mu   sync.RWMutex
	info UpdateInfo
	// Track whether we already logged the discovery (log only once)
	loggedStable bool
	loggedBeta   bool
}

// version represents a parsed semantic version.
type version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // e.g. "beta.6", empty for stable
}

// githubRelease is the subset of fields we need from the GitHub Releases API.
type githubRelease struct {
	TagName string `json:"tag_name"` // e.g. "v1.2.0" or "v1.2.0-beta.6"
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
}

// parseVersion parses a version string like "1.2.0" or "1.2.0-beta.6" (with or without "v" prefix).
func parseVersion(s string) (version, error) {
	s = strings.TrimPrefix(s, "v")
	var v version

	// Split on "-" to separate prerelease
	parts := strings.SplitN(s, "-", 2)
	if len(parts) == 2 {
		v.Prerelease = parts[1]
	}

	// Parse major.minor.patch
	nums := strings.Split(parts[0], ".")
	if len(nums) != 3 {
		return v, fmt.Errorf("invalid version format: %s", s)
	}

	var err error
	v.Major, err = strconv.Atoi(nums[0])
	if err != nil {
		return v, fmt.Errorf("invalid major version: %s", nums[0])
	}
	v.Minor, err = strconv.Atoi(nums[1])
	if err != nil {
		return v, fmt.Errorf("invalid minor version: %s", nums[1])
	}
	v.Patch, err = strconv.Atoi(nums[2])
	if err != nil {
		return v, fmt.Errorf("invalid patch version: %s", nums[2])
	}

	return v, nil
}

// isStable returns true if this version has no prerelease suffix.
func (v version) isStable() bool {
	return v.Prerelease == ""
}

// String returns the version as a string (without "v" prefix).
func (v version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// compareVersions compares two versions.
// Returns: -1 if a < b, 0 if a == b, 1 if a > b.
// At the same major.minor.patch, stable > prerelease.
// Prerelease comparison is numeric when possible (beta.6 > beta.2).
func compareVersions(a, b version) int {
	// Compare major.minor.patch
	if a.Major != b.Major {
		return intCmp(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return intCmp(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return intCmp(a.Patch, b.Patch)
	}

	// Same base version — stable beats prerelease
	if a.isStable() && !b.isStable() {
		return 1
	}
	if !a.isStable() && b.isStable() {
		return -1
	}
	if a.isStable() && b.isStable() {
		return 0
	}

	// Both have prerelease — compare numerically where possible
	return comparePrerelease(a.Prerelease, b.Prerelease)
}

// comparePrerelease compares prerelease strings like "beta.6" vs "beta.2".
func comparePrerelease(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	max := len(aParts)
	if len(bParts) > max {
		max = len(bParts)
	}

	for i := 0; i < max; i++ {
		var aStr, bStr string
		if i < len(aParts) {
			aStr = aParts[i]
		}
		if i < len(bParts) {
			bStr = bParts[i]
		}

		// Try numeric comparison
		aNum, aErr := strconv.Atoi(aStr)
		bNum, bErr := strconv.Atoi(bStr)
		if aErr == nil && bErr == nil {
			if aNum != bNum {
				return intCmp(aNum, bNum)
			}
			continue
		}

		// Fall back to string comparison
		if aStr < bStr {
			return -1
		}
		if aStr > bStr {
			return 1
		}
	}

	return 0
}

func intCmp(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// updateCheckLoop runs the background update check goroutine.
func (f *Farmer) updateCheckLoop() {
	// Initial check after 10 seconds
	timer := time.NewTimer(10 * time.Second)
	select {
	case <-timer.C:
		f.checkForUpdates()
	case <-f.stopCh:
		timer.Stop()
		return
	}

	// Then every 6 hours
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f.checkForUpdates()
		case <-f.stopCh:
			return
		}
	}
}

// checkForUpdates queries GitHub releases and updates the internal state.
func (f *Farmer) checkForUpdates() {
	current, err := parseVersion(f.version)
	if err != nil {
		return // Can't compare if we don't know our own version
	}

	releases, err := fetchGitHubReleases()
	if err != nil {
		f.writeLogFile(fmt.Sprintf("[Update] GitHub API error: %v", err))
		return // Silent failure, retry next cycle
	}

	var latestStable, latestBeta version
	var stableURL, betaURL string
	stableFound, betaFound := false, false

	for _, rel := range releases {
		if rel.Draft {
			continue
		}

		v, err := parseVersion(rel.TagName)
		if err != nil {
			continue
		}

		if v.isStable() {
			if !stableFound || compareVersions(v, latestStable) > 0 {
				latestStable = v
				stableURL = rel.HTMLURL
				stableFound = true
			}
		} else {
			if !betaFound || compareVersions(v, latestBeta) > 0 {
				latestBeta = v
				betaURL = rel.HTMLURL
				betaFound = true
			}
		}
	}

	f.update.mu.Lock()
	defer f.update.mu.Unlock()

	f.update.info = UpdateInfo{
		CurrentVersion: current.String(),
		IsBeta:         !current.isStable(),
	}

	// Check for newer stable
	if stableFound && compareVersions(latestStable, current) > 0 {
		f.update.info.HasStableUpdate = true
		f.update.info.LatestStable = latestStable.String()
		f.update.info.StableURL = stableURL

		if !f.update.loggedStable {
			f.update.loggedStable = true
			f.addLog("[Update] New stable version available: v%s — %s", latestStable.String(), stableURL)
		}
	}

	// Beta user: also check for newer beta
	if !current.isStable() && betaFound && compareVersions(latestBeta, current) > 0 {
		f.update.info.HasBetaUpdate = true
		f.update.info.LatestBeta = latestBeta.String()
		f.update.info.BetaURL = betaURL

		if !f.update.loggedBeta {
			f.update.loggedBeta = true
			f.addLog("[Update] New beta version available: v%s — %s", latestBeta.String(), betaURL)
		}
	}
}

// GetUpdateInfo returns a copy of the current update state.
func (f *Farmer) GetUpdateInfo() UpdateInfo {
	f.update.mu.RLock()
	defer f.update.mu.RUnlock()
	return f.update.info
}

// fetchGitHubReleases fetches the latest releases from the GitHub API.
func fetchGitHubReleases() ([]githubRelease, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/miwidot/twitchpoint/releases?per_page=20", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}

	return releases, nil
}
