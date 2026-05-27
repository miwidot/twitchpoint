package twitch

import (
	"fmt"
	"time"
)

// GQL query for fetching ALL available drop campaigns via the Viewer Drops Dashboard.
// Returns every campaign the user is eligible for, including ones not yet started.
// Requires Android App Client-ID (kd1unb4b3q4t58fwlpcbzcbnm76a8fp) — returns null with TV Client-ID.
const queryDropsDashboard = `query ViewerDropsDashboard {
	currentUser {
		dropCampaigns {
			id
			name
			status
			game {
				id
				displayName
			}
			startAt
			endAt
			self {
				isAccountConnected
			}
			timeBasedDrops {
				id
				name
				startAt
				endAt
				requiredMinutesWatched
				preconditionDrops {
					id
				}
				benefitEdges {
					benefit {
						id
						name
						imageAssetURL
					}
				}
				self {
					currentMinutesWatched
					dropInstanceID
					isClaimed
				}
			}
			allow {
				channels {
					id
					name
					displayName
				}
			}
		}
	}
}`

// GQL query for fetching campaigns with progress + gameEventDrops (claimed benefit history).
// gameEventDrops contains ALL ever-claimed benefit IDs with lastAwardedAt timestamps.
// This is key for detecting already-completed campaigns that disappear from dropCampaignsInProgress.
const queryDropsInventory = `query Inventory {
	currentUser {
		inventory {
			gameEventDrops {
				id
				lastAwardedAt
			}
			dropCampaignsInProgress {
				id
				name
				status
				game {
					id
					displayName
				}
				startAt
				endAt
				self {
					isAccountConnected
				}
				timeBasedDrops {
					id
					name
					startAt
					endAt
					requiredMinutesWatched
					preconditionDrops {
						id
					}
					benefitEdges {
						benefit {
							id
							name
							imageAssetURL
						}
					}
					self {
						currentMinutesWatched
						dropInstanceID
						isClaimed
					}
				}
				allow {
					channels {
						id
						name
						displayName
					}
				}
			}
		}
	}
}`

// GQL mutation to claim a completed drop reward.
const mutationClaimDropRewards = `mutation DropsPage_ClaimDropRewards($input: ClaimDropRewardsInput!) {
	claimDropRewards(input: $input) {
		status
	}
}`

// DropCampaign represents an active drop campaign from the inventory.
type DropCampaign struct {
	ID                 string
	Name               string
	Status             string // ACTIVE, EXPIRED, etc.
	GameName           string
	GameID             string
	GameSlug           string // URL slug (e.g. "escape-from-tarkov"); required by the GameDirectory persisted query
	StartAt            time.Time
	EndAt              time.Time
	IsAccountConnected bool // whether the user's account is linked for this game
	InInventory        bool // true if campaign appeared in Inventory (has/had progress)
	Drops              []TimeBasedDrop
	Channels           []DropChannel // allowed channels (empty = any channel with the game)
}

// TimeBasedDrop represents a single drop within a campaign.
type TimeBasedDrop struct {
	ID                     string
	Name                   string
	RequiredMinutesWatched int
	CurrentMinutesWatched  int
	DropInstanceID         string // non-empty when progress exists
	IsClaimed              bool
	BenefitID              string    // benefit ID for cross-referencing with gameEventDrops
	BenefitName            string
	BenefitType            string    // distributionType: BADGE / EMOTE / DIRECT_ENTITLEMENT / UNKNOWN. Drives the "earnable without account-link" check (badges/emotes are Twitch-side rewards, no publisher account needed).
	StartAt                time.Time // per-drop time window — drop only earnable from StartAt
	EndAt                  time.Time // per-drop time window — drop only earnable until EndAt
	PreconditionDrops      []string  // IDs of drops that must be claimed before this one is earnable
}

// IsEarnable returns true when the drop is currently earnable on Twitch's
// side. Mirrors TDM's TimedDrop._base_can_earn — checks the drop's own
// time window plus the preconditions chain (every precondition drop must
// already be claimed). Caller passes the campaign's full drop list so we
// can resolve preconditionDrops IDs back to their is_claimed state.
//
// Without this filter, the bot picks campaigns whose first-unclaimed drop
// is technically still in inventory but Twitch refuses to credit (drop
// hasn't started yet, or its precondition isn't claimed). Symptom:
// getCurrentDropSession returns nil and the bot wastes a pick window.
func (d *TimeBasedDrop) IsEarnable(now time.Time, campaignDrops []TimeBasedDrop) bool {
	if d.IsClaimed {
		return false
	}
	if d.RequiredMinutesWatched <= 0 {
		return false
	}
	// Time window. A zero StartAt is treated as "always started" (legacy
	// drops without per-drop timestamps); zero EndAt likewise means
	// "no end" — fall back to the campaign-level window in callers if
	// stricter checking is needed.
	if !d.StartAt.IsZero() && now.Before(d.StartAt) {
		return false
	}
	if !d.EndAt.IsZero() && !now.Before(d.EndAt) {
		return false
	}
	// Preconditions: every precondition drop must already be claimed.
	if len(d.PreconditionDrops) > 0 {
		claimed := make(map[string]bool, len(campaignDrops))
		for _, cd := range campaignDrops {
			if cd.IsClaimed {
				claimed[cd.ID] = true
			}
		}
		for _, pid := range d.PreconditionDrops {
			if !claimed[pid] {
				return false
			}
		}
	}
	return true
}

// DropChannel represents a channel allowed for a drop campaign.
type DropChannel struct {
	ID          string
	Name        string
	DisplayName string
}

// IsComplete returns true if the drop has reached 100% watch time.
func (d *TimeBasedDrop) IsComplete() bool {
	return d.RequiredMinutesWatched > 0 && d.CurrentMinutesWatched >= d.RequiredMinutesWatched
}

// ProgressPercent returns the drop progress as a percentage (0-100).
func (d *TimeBasedDrop) ProgressPercent() int {
	if d.RequiredMinutesWatched <= 0 {
		return 0
	}
	pct := (d.CurrentMinutesWatched * 100) / d.RequiredMinutesWatched
	if pct > 100 {
		pct = 100
	}
	return pct
}

// GetDropsInventory fetches ALL available drop campaigns with progress data.
// Step 1: ViewerDropsDashboard → all campaigns the user is eligible for (no progress).
// Step 2: Inventory → campaigns with progress + gameEventDrops (all ever-claimed benefit IDs).
// Step 3: Merge progress from Inventory into Dashboard campaigns.
// Step 4: Use gameEventDrops to detect already-completed drops not in Inventory.
// Falls back to Inventory-only if Dashboard fails.
func (g *GQLClient) GetDropsInventory() ([]DropCampaign, error) {
	dashboardCampaigns, err := g.getDropsDashboard()
	if err != nil || dashboardCampaigns == nil {
		// Fallback: inventory only (still has progress data)
		if err != nil {
			g.diag("[Drops/Diag] Dashboard query failed (%v) — falling back to Inventory-only. Campaigns without prior progress will be invisible until user generates first watch-minute (e.g. opens twitch.tv channel in browser).", err)
		} else {
			g.diag("[Drops/Diag] Dashboard returned nil/empty — falling back to Inventory-only. Campaigns without prior progress will be invisible until user generates first watch-minute (e.g. opens twitch.tv channel in browser).")
		}
		campaigns, _, invErr := g.getDropsFromInventory()
		if invErr != nil {
			return nil, invErr
		}
		return campaigns, nil
	}

	// Fetch inventory for progress data + gameEventDrops
	inventoryCampaigns, claimedBenefits, _ := g.getDropsFromInventory()

	// One-shot diag: dump every inventory campaign so we can spot
	// "should-be-active campaign missing or mislabeled" cases.
	for _, ic := range inventoryCampaigns {
		claimedCount := 0
		totalDrops := 0
		for _, d := range ic.Drops {
			totalDrops++
			if d.IsClaimed {
				claimedCount++
			}
		}
		g.diag("[Drops/Diag] inventory entry: name=%q game=%q status=%q connected=%t drops=%d claimed=%d endAt=%s",
			ic.Name, ic.GameName, ic.Status, ic.IsAccountConnected, totalDrops, claimedCount, ic.EndAt.Format(time.RFC3339))
	}

	// Step 2.5: Dashboard only returns campaign summaries (no timeBasedDrops,
	// no allow.channels) under the persisted hash. For any campaign that's
	// NOT already in the inventory (= no progress yet), we need to fetch its
	// full detail explicitly via DropCampaignDetails. Limit to ACTIVE /
	// UPCOMING campaigns whose window hasn't expired — avoids burning
	// batch slots on dead campaigns.
	inventoryCampaignIDs := make(map[string]bool, len(inventoryCampaigns))
	for _, ic := range inventoryCampaigns {
		inventoryCampaignIDs[ic.ID] = true
	}

	now := time.Now()
	var needDetails []string
	for _, dc := range dashboardCampaigns {
		if inventoryCampaignIDs[dc.ID] {
			continue
		}
		if dc.Status != "" && dc.Status != "ACTIVE" && dc.Status != "UPCOMING" {
			continue
		}
		if !dc.EndAt.IsZero() && !dc.EndAt.After(now) {
			continue
		}
		needDetails = append(needDetails, dc.ID)
	}

	detailsByID, detailsErr := g.getCampaignDetails(needDetails)
	if detailsErr != nil {
		g.diag("[Drops/Diag] CampaignDetails partial failure (%v) — got %d/%d, continuing with what we have", detailsErr, len(detailsByID), len(needDetails))
	}

	g.diag("[Drops/Merge] Dashboard=%d campaigns, Inventory=%d campaigns, Details fetched=%d/%d, gameEventDrops=%d benefits",
		len(dashboardCampaigns), len(inventoryCampaigns), len(detailsByID), len(needDetails), len(claimedBenefits))

	// Rebuild lookups now that we have details
	progressByDropID := make(map[string]TimeBasedDrop)
	for _, ic := range inventoryCampaigns {
		for _, drop := range ic.Drops {
			progressByDropID[drop.ID] = drop
		}
	}

	// Step 3: Merge detail/inventory data into the Dashboard summaries.
	// Priority: inventory (has progress) > details (no progress) > dashboard
	// summary (useless on its own — no drops/channels).
	for i := range dashboardCampaigns {
		id := dashboardCampaigns[i].ID

		if inventoryCampaignIDs[id] {
			dashboardCampaigns[i].InInventory = true
			// Inventory entries have full detail AND progress. Replace the
			// summary with them so the selector sees timeBasedDrops/channels.
			for _, ic := range inventoryCampaigns {
				if ic.ID == id {
					ic.InInventory = true
					dashboardCampaigns[i] = ic
					break
				}
			}
		} else if details, ok := detailsByID[id]; ok {
			// Details query returned full detail; replace summary. No progress
			// to merge (the campaign isn't in inventory).
			dashboardCampaigns[i] = details
		}

		// Step 3b: Merge per-drop progress from inventory (handles the case
		// where Details and Inventory both have the drop ID — Inventory's
		// progress is authoritative).
		for j := range dashboardCampaigns[i].Drops {
			drop := &dashboardCampaigns[i].Drops[j]
			if inv, ok := progressByDropID[drop.ID]; ok {
				drop.CurrentMinutesWatched = inv.CurrentMinutesWatched
				drop.DropInstanceID = inv.DropInstanceID
				drop.IsClaimed = inv.IsClaimed
			}
		}

		// Step 4: gameEventDrops claim-detection fallback, time-window
		// scoped. After a recent claim Twitch's dashboard sometimes
		// keeps reporting `self.isClaimed=false` for a few seconds to
		// minutes (the same window where getCurrentDropSession returns
		// nil), so the bot otherwise re-picks a campaign whose drop
		// is actually already claimed. gameEventDrops is the
		// permanent benefit-award history and reflects the claim
		// immediately.
		//
		// The v1.8.0 regression we're avoiding: marking a daily-
		// rolling drop (Marble Day, etc.) as already-claimed because
		// yesterday's instance was awarded under the same benefit ID.
		// Fix: only honour the fallback when lastAwardedAt is inside
		// THIS drop's [StartAt, EndAt) window. That ties the claim
		// signal to the actual instance, not the benefit ID alone —
		// matches TDM's approach (src/models/drop.py:49 reads
		// gameEventDrops timestamp and compares against the drop's
		// own period).
		for j := range dashboardCampaigns[i].Drops {
			drop := &dashboardCampaigns[i].Drops[j]
			if drop.IsClaimed || drop.BenefitID == "" {
				continue
			}
			awarded, ok := claimedBenefits[drop.BenefitID]
			if !ok {
				continue
			}
			// If the drop has explicit per-drop windows, the award
			// must fall inside [StartAt, EndAt). Without windows,
			// fall back to the campaign window so daily-rolling
			// campaigns don't false-positive.
			start := drop.StartAt
			end := drop.EndAt
			if start.IsZero() {
				start = dashboardCampaigns[i].StartAt
			}
			if end.IsZero() {
				end = dashboardCampaigns[i].EndAt
			}
			if start.IsZero() || end.IsZero() {
				continue // no window to check, keep dashboard truth
			}
			if (awarded.Equal(start) || awarded.After(start)) && awarded.Before(end) {
				drop.IsClaimed = true
			}
		}
	}

	// Step 5: union with Inventory campaigns that the Dashboard summary
	// didn't include. Twitch's persisted-hash Dashboard occasionally
	// omits in-progress campaigns once they reach late-stage state
	// (partially claimed, late-day rotation, etc.). Inventory still
	// carries them — and we want to keep farming them. We treat Inventory
	// as the truth base and union Dashboard/Details INTO it instead of the
	// other way around (the dashboard-only approach silently drops late-
	// stage progress campaigns).
	dashboardCampaignIDs := make(map[string]bool, len(dashboardCampaigns))
	for _, dc := range dashboardCampaigns {
		dashboardCampaignIDs[dc.ID] = true
	}
	for _, ic := range inventoryCampaigns {
		if dashboardCampaignIDs[ic.ID] {
			continue
		}
		ic.InInventory = true
		dashboardCampaigns = append(dashboardCampaigns, ic)
	}

	return dashboardCampaigns, nil
}

// Persisted-query SHA256 hashes for the Twitch GraphQL operations we use
// for drops discovery. Switching from raw Query strings to persisted-hash
// requests aligns us with the Android-app's canonical traffic; raw queries
// produce non-deterministic responses (dropCampaigns sometimes returns null
// even for accounts that have eligible campaigns), persisted-hash responses
// are stable. Hashes verified against current Twitch Android-app traffic.
const (
	persistedHashViewerDropsDashboard = "5a4da2ab3d5b47c9f9ce864e727b2cb346af1e3ea8b897fe8f704a97ff017619"
	persistedHashInventory            = "d86775d0ef16a63a33ad52e80eaff963b2d5b72fada7c991504a57496e1d8e4b"
	// persistedHashCampaignDetails returns the full per-campaign object
	// (timeBasedDrops, allow.channels, self.isAccountConnected, etc.) under
	// data.user.dropCampaign. Used to enrich the Dashboard summary list.
	persistedHashCampaignDetails = "039277bf98f3130929262cc7c6efd9c141ca3749cb6dca442fc8ead9a53f77c1"

	// campaignDetailsBatchSize: Twitch caps batched GQL requests around 35;
	// 20 keeps us well under that ceiling while still cutting the round-trip
	// count by 20x compared to per-campaign calls.
	campaignDetailsBatchSize = 20
)

// getCampaignDetails fetches full campaign data for the given IDs in batched
// requests. Used to enrich the Dashboard summary (which lacks timeBasedDrops
// and allow.channels) for campaigns that aren't already covered by Inventory.
//
// Returns a map keyed by campaign ID. Campaigns that fail to parse are
// omitted; the function logs but does not error on partial failures so a
// single bad campaign can't kill the whole refresh cycle.
func (g *GQLClient) getCampaignDetails(campaignIDs []string) (map[string]DropCampaign, error) {
	if len(campaignIDs) == 0 {
		return map[string]DropCampaign{}, nil
	}
	if g.userID == "" {
		return nil, fmt.Errorf("getCampaignDetails: userID not set (call SetUserID first)")
	}

	out := make(map[string]DropCampaign, len(campaignIDs))

	for start := 0; start < len(campaignIDs); start += campaignDetailsBatchSize {
		end := start + campaignDetailsBatchSize
		if end > len(campaignIDs) {
			end = len(campaignIDs)
		}
		chunk := campaignIDs[start:end]

		reqs := make([]GQLRequest, len(chunk))
		for i, id := range chunk {
			reqs[i] = GQLRequest{
				OperationName: "DropCampaignDetails",
				Variables: map[string]interface{}{
					// channelLogin variable accepts the user_id as a string here
					// despite the field name — Twitch backend handles both forms.
					"channelLogin": g.userID,
					"dropID":       id,
				},
				Extensions: &GQLExtensions{
					PersistedQuery: &PersistedQuery{
						Version:    1,
						SHA256Hash: persistedHashCampaignDetails,
					},
				},
			}
		}

		resps, err := g.doBatch(reqs)
		if err != nil {
			return out, fmt.Errorf("campaign details batch %d-%d: %w", start, end, err)
		}

		for i, resp := range resps {
			id := chunk[i]
			userRaw, ok := resp.Data["user"]
			if !ok || userRaw == nil {
				continue
			}
			userMap, ok := userRaw.(map[string]interface{})
			if !ok {
				continue
			}
			campaignRaw, ok := userMap["dropCampaign"]
			if !ok || campaignRaw == nil {
				continue
			}
			parsed := parseCampaignList([]interface{}{campaignRaw})
			if len(parsed) == 0 {
				continue
			}
			out[id] = parsed[0]
		}
	}

	return out, nil
}

// getDropsDashboard fetches all campaigns via ViewerDropsDashboard using the
// persisted-query hash. NOTE: this hash returns only campaign *summaries* —
// id/name/game/status/endAt/self.isAccountConnected — NOT the nested drops
// or allow.channels. Callers must enrich with DropCampaignDetails for any
// campaign whose full data isn't already covered by the Inventory response.
// See GetDropsInventory for the merge flow.
func (g *GQLClient) getDropsDashboard() ([]DropCampaign, error) {
	req := &GQLRequest{
		OperationName: "ViewerDropsDashboard",
		Variables: map[string]interface{}{
			"fetchRewardCampaigns": false,
		},
		Extensions: &GQLExtensions{
			PersistedQuery: &PersistedQuery{
				Version:    1,
				SHA256Hash: persistedHashViewerDropsDashboard,
			},
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get drops dashboard: %w", err)
	}

	currentUser, ok := resp.Data["currentUser"]
	if !ok || currentUser == nil {
		g.diag("[Drops/Diag] Dashboard: currentUser missing/null in response (auth token may lack scope, or Client-ID rejected). Raw data keys: %v", mapKeys(resp.Data))
		return nil, nil
	}
	userMap, ok := currentUser.(map[string]interface{})
	if !ok {
		g.diag("[Drops/Diag] Dashboard: currentUser is not an object (type=%T)", currentUser)
		return nil, nil
	}

	campaignsRaw, ok := userMap["dropCampaigns"]
	if !ok || campaignsRaw == nil {
		g.diag("[Drops/Diag] Dashboard: dropCampaigns missing/null (Twitch returned the user but no campaigns — A/B test, region, or account-state issue). currentUser keys: %v", mapKeys(userMap))
		return nil, nil
	}
	campaignList, ok := campaignsRaw.([]interface{})
	if !ok {
		g.diag("[Drops/Diag] Dashboard: dropCampaigns is not a list (type=%T)", campaignsRaw)
		return nil, nil
	}

	parsed := parseCampaignList(campaignList)
	g.diag("[Drops/Diag] Dashboard OK: %d campaigns parsed from %d raw entries", len(parsed), len(campaignList))
	return parsed, nil
}

// parseDiagOnce dumps the field shape of the first campaign each call. It
// routes through the package-level diagSink so the output ends up in the
// user-visible file log instead of the discarded log.Printf stream.
func parseDiagOnce(cMap map[string]interface{}) {
	diag := getParseDiag()
	if diag == nil {
		return
	}
	drops := -1
	if td, ok := cMap["timeBasedDrops"].([]interface{}); ok {
		drops = len(td)
	} else if _, present := cMap["timeBasedDrops"]; !present {
		drops = -2 // key missing entirely
	}
	channels := -1
	if allow, ok := cMap["allow"].(map[string]interface{}); ok {
		if ch, ok := allow["channels"].([]interface{}); ok {
			channels = len(ch)
		} else if _, present := allow["channels"]; !present {
			channels = -2
		}
	}
	diag("[Drops/Diag] First campaign raw keys=%v | timeBasedDrops count=%d (-1 wrong type, -2 missing) | allow.channels count=%d | name=%q endAt=%q status=%q",
		mapKeys(cMap), drops, channels,
		getString(cMap, "name"), getString(cMap, "endAt"), getString(cMap, "status"))
}

// parseDiagSink is the package-level sink for parseCampaignList diagnostics.
// Set by GQLClient when it has a DiagLog wired. Package-level (not method) so
// the static parser helpers can reach it without changing signatures.
var parseDiagSink func(format string, args ...interface{})

func getParseDiag() func(format string, args ...interface{}) { return parseDiagSink }

// SetParseDiagSink wires the file-logger sink for parser diagnostics.
func SetParseDiagSink(sink func(format string, args ...interface{})) {
	parseDiagSink = sink
}

// mapKeys returns the keys of a map[string]interface{} for diagnostic logging.
func mapKeys(m map[string]interface{}) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// getDropsFromInventory fetches campaigns via the Inventory query (fallback).
// Also returns gameEventDrops: a map of benefitID → lastAwardedAt for ALL ever-claimed benefits.
func (g *GQLClient) getDropsFromInventory() ([]DropCampaign, map[string]time.Time, error) {
	req := &GQLRequest{
		OperationName: "Inventory",
		Variables: map[string]interface{}{
			"fetchRewardCampaigns": false,
		},
		Extensions: &GQLExtensions{
			PersistedQuery: &PersistedQuery{
				Version:    1,
				SHA256Hash: persistedHashInventory,
			},
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("get drops inventory: %w", err)
	}

	currentUser, ok := resp.Data["currentUser"]
	if !ok || currentUser == nil {
		return nil, nil, nil
	}
	userMap, ok := currentUser.(map[string]interface{})
	if !ok {
		return nil, nil, nil
	}

	inventory, ok := userMap["inventory"]
	if !ok || inventory == nil {
		return nil, nil, nil
	}
	invMap, ok := inventory.(map[string]interface{})
	if !ok {
		return nil, nil, nil
	}

	// Parse gameEventDrops — permanent history of all claimed benefit IDs
	claimedBenefits := parseGameEventDrops(invMap)

	campaignsRaw, ok := invMap["dropCampaignsInProgress"]
	if !ok || campaignsRaw == nil {
		return nil, claimedBenefits, nil
	}
	campaignList, ok := campaignsRaw.([]interface{})
	if !ok {
		return nil, claimedBenefits, nil
	}

	return parseCampaignList(campaignList), claimedBenefits, nil
}

// parseGameEventDrops extracts the benefit ID → lastAwardedAt map from the inventory response.
// gameEventDrops contains ALL benefits ever claimed by the user, even from completed/expired campaigns.
func parseGameEventDrops(invMap map[string]interface{}) map[string]time.Time {
	result := make(map[string]time.Time)

	geDropsRaw, ok := invMap["gameEventDrops"]
	if !ok || geDropsRaw == nil {
		return result
	}
	geDropsList, ok := geDropsRaw.([]interface{})
	if !ok {
		return result
	}

	for _, item := range geDropsList {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := getString(itemMap, "id")
		if id == "" {
			continue
		}
		lastAwarded := getString(itemMap, "lastAwardedAt")
		if lastAwarded != "" {
			if t, err := time.Parse(time.RFC3339, lastAwarded); err == nil {
				result[id] = t
			}
		} else {
			// Benefit exists but no timestamp — still claimed
			result[id] = time.Time{}
		}
	}

	return result
}

// parseCampaignList parses a list of campaign objects from GQL response.
func parseCampaignList(campaignList []interface{}) []DropCampaign {
	var campaigns []DropCampaign
	for i, cRaw := range campaignList {
		cMap, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// One-time diagnostic dump of the first campaign's raw shape to
		// verify which fields the persisted-hash response actually carries.
		// Remove once the field-shape question is resolved.
		if i == 0 {
			parseDiagOnce(cMap)
		}

		campaign := DropCampaign{
			ID:                 getString(cMap, "id"),
			Name:               getString(cMap, "name"),
			Status:             getString(cMap, "status"),
			IsAccountConnected: true, // default true — only false when API explicitly says so
		}

		// Parse game with `displayName || name` fallback. The persisted-hash
		// Inventory and CampaignDetails responses return only `name` (Android
		// schema), while the raw-query Dashboard returned `displayName`.
		// Without the fallback every inventory campaign ends up with an empty
		// GameName and gets silently rejected by the wanted_games whitelist.
		if game, ok := cMap["game"]; ok && game != nil {
			if gMap, ok := game.(map[string]interface{}); ok {
				campaign.GameID = getString(gMap, "id")
				if dn := getString(gMap, "displayName"); dn != "" {
					campaign.GameName = dn
				} else {
					campaign.GameName = getString(gMap, "name")
				}
				campaign.GameSlug = getString(gMap, "slug")
			}
		}

		// Parse account connection status (only override if explicitly false)
		if self, ok := cMap["self"]; ok && self != nil {
			if selfMap, ok := self.(map[string]interface{}); ok {
				if connected, ok := selfMap["isAccountConnected"]; ok && connected != nil {
					if b, ok := connected.(bool); ok {
						campaign.IsAccountConnected = b
					}
				}
			}
		}

		// Parse timestamps
		if startAt := getString(cMap, "startAt"); startAt != "" {
			if t, err := time.Parse(time.RFC3339, startAt); err == nil {
				campaign.StartAt = t
			}
		}
		if endAt := getString(cMap, "endAt"); endAt != "" {
			if t, err := time.Parse(time.RFC3339, endAt); err == nil {
				campaign.EndAt = t
			}
		}

		// Parse time-based drops
		if dropsRaw, ok := cMap["timeBasedDrops"]; ok && dropsRaw != nil {
			if dropList, ok := dropsRaw.([]interface{}); ok {
				for _, dRaw := range dropList {
					dMap, ok := dRaw.(map[string]interface{})
					if !ok {
						continue
					}

					drop := TimeBasedDrop{
						ID:                    getString(dMap, "id"),
						Name:                  getString(dMap, "name"),
						RequiredMinutesWatched: getInt(dMap, "requiredMinutesWatched"),
					}

					// Parse per-drop time window (TDM parity — a drop's
					// startAt/endAt may differ from the campaign's window
					// for multi-drop chains).
					if startAt := getString(dMap, "startAt"); startAt != "" {
						if t, err := time.Parse(time.RFC3339, startAt); err == nil {
							drop.StartAt = t
						}
					}
					if endAt := getString(dMap, "endAt"); endAt != "" {
						if t, err := time.Parse(time.RFC3339, endAt); err == nil {
							drop.EndAt = t
						}
					}

					// Parse preconditionDrops list (drops that must be
					// claimed before this one becomes earnable).
					if pre, ok := dMap["preconditionDrops"]; ok && pre != nil {
						if preList, ok := pre.([]interface{}); ok {
							for _, pItem := range preList {
								if pMap, ok := pItem.(map[string]interface{}); ok {
									if pid := getString(pMap, "id"); pid != "" {
										drop.PreconditionDrops = append(drop.PreconditionDrops, pid)
									}
								}
							}
						}
					}

					// Parse benefit name
					if benefitEdges, ok := dMap["benefitEdges"]; ok && benefitEdges != nil {
						if edges, ok := benefitEdges.([]interface{}); ok && len(edges) > 0 {
							if edge, ok := edges[0].(map[string]interface{}); ok {
								if benefit, ok := edge["benefit"]; ok && benefit != nil {
									if bMap, ok := benefit.(map[string]interface{}); ok {
										drop.BenefitID = getString(bMap, "id")
									drop.BenefitName = getString(bMap, "name")
									drop.BenefitType = getString(bMap, "distributionType")
									}
								}
							}
						}
					}

					// Parse self (progress)
					if self, ok := dMap["self"]; ok && self != nil {
						if sMap, ok := self.(map[string]interface{}); ok {
							drop.CurrentMinutesWatched = getInt(sMap, "currentMinutesWatched")
							drop.DropInstanceID = getString(sMap, "dropInstanceID")
							if claimed, ok := sMap["isClaimed"]; ok && claimed != nil {
								if b, ok := claimed.(bool); ok {
									drop.IsClaimed = b
								}
							}
						}
					}

					campaign.Drops = append(campaign.Drops, drop)
				}
			}
		}

		// Parse allowed channels
		if allow, ok := cMap["allow"]; ok && allow != nil {
			if allowMap, ok := allow.(map[string]interface{}); ok {
				if chRaw, ok := allowMap["channels"]; ok && chRaw != nil {
					if chList, ok := chRaw.([]interface{}); ok {
						for _, chItem := range chList {
							if chMap, ok := chItem.(map[string]interface{}); ok {
								campaign.Channels = append(campaign.Channels, DropChannel{
									ID:          getString(chMap, "id"),
									Name:        getString(chMap, "name"),
									DisplayName: getString(chMap, "displayName"),
								})
							}
						}
					}
				}
			}
		}

		campaigns = append(campaigns, campaign)
	}

	return campaigns
}

// ClaimDrop claims a completed drop by its instance ID.
func (g *GQLClient) ClaimDrop(dropInstanceID string) error {
	req := &GQLRequest{
		OperationName: "DropsPage_ClaimDropRewards",
		Query:         mutationClaimDropRewards,
		Variables: map[string]interface{}{
			"input": map[string]interface{}{
				"dropInstanceID": dropInstanceID,
			},
		},
	}

	resp, err := g.do(req)
	if err != nil {
		return fmt.Errorf("claim drop: %w", err)
	}

	if resp != nil {
		if claimData, ok := resp.Data["claimDropRewards"]; ok && claimData != nil {
			if cdMap, ok := claimData.(map[string]interface{}); ok {
				status := getString(cdMap, "status")
				if status != "" && status != "ELIGIBLE_FOR_ALL" && status != "CLAIMED" {
					return fmt.Errorf("claim drop returned status: %s", status)
				}
			}
		}
	}

	return nil
}
