package twitch

import (
	"fmt"
	"log"
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
				requiredMinutesWatched
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
					requiredMinutesWatched
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
	BenefitID              string // benefit ID for cross-referencing with gameEventDrops
	BenefitName            string
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
		campaigns, _, invErr := g.getDropsFromInventory()
		if invErr != nil {
			return nil, invErr
		}
		return campaigns, nil
	}

	// Fetch inventory for progress data + gameEventDrops
	inventoryCampaigns, claimedBenefits, _ := g.getDropsFromInventory()

	log.Printf("[Drops/Merge] Dashboard=%d campaigns, Inventory=%d campaigns, gameEventDrops=%d benefits",
		len(dashboardCampaigns), len(inventoryCampaigns), len(claimedBenefits))

	// Build lookups from inventory campaigns
	inventoryCampaignIDs := make(map[string]bool)
	progressByDropID := make(map[string]TimeBasedDrop)
	for _, ic := range inventoryCampaigns {
		inventoryCampaignIDs[ic.ID] = true
		for _, drop := range ic.Drops {
			progressByDropID[drop.ID] = drop
		}
	}

	// Merge progress into dashboard campaigns
	for i := range dashboardCampaigns {
		if inventoryCampaignIDs[dashboardCampaigns[i].ID] {
			dashboardCampaigns[i].InInventory = true
		}
		for j := range dashboardCampaigns[i].Drops {
			drop := &dashboardCampaigns[i].Drops[j]

			// Step 3: Merge progress from inventory (in-progress campaigns)
			if inv, ok := progressByDropID[drop.ID]; ok {
				drop.CurrentMinutesWatched = inv.CurrentMinutesWatched
				drop.DropInstanceID = inv.DropInstanceID
				drop.IsClaimed = inv.IsClaimed
				continue
			}

			// Step 4: Use gameEventDrops to detect already-claimed drops.
			// If a drop's benefit ID appears in gameEventDrops AND the lastAwardedAt
			// falls within the campaign's time window, the drop was already claimed.
			// This catches campaigns that disappeared from dropCampaignsInProgress.
			if drop.BenefitID != "" && len(claimedBenefits) > 0 {
				if lastAwarded, found := claimedBenefits[drop.BenefitID]; found {
					campaign := dashboardCampaigns[i]
					if isWithinCampaignWindow(lastAwarded, campaign.StartAt, campaign.EndAt) {
						drop.IsClaimed = true
						drop.CurrentMinutesWatched = drop.RequiredMinutesWatched
						log.Printf("[Drops/Merge] gameEventDrops: marked %q drop %q as claimed (benefit=%s, awarded=%s)",
							campaign.Name, drop.Name, drop.BenefitID, lastAwarded.Format(time.RFC3339))
					} else {
						log.Printf("[Drops/Merge] gameEventDrops: benefit %s found but outside window (awarded=%s, campaign=%s..%s)",
							drop.BenefitID, lastAwarded.Format(time.RFC3339),
							campaign.StartAt.Format(time.RFC3339), campaign.EndAt.Format(time.RFC3339))
					}
				}
			}
		}
	}

	return dashboardCampaigns, nil
}

// isWithinCampaignWindow checks if an award timestamp falls within the campaign's time window.
// Returns true if we can't determine the window (missing timestamps) — benefit of the doubt.
func isWithinCampaignWindow(awarded, campaignStart, campaignEnd time.Time) bool {
	if awarded.IsZero() {
		// Benefit was claimed but no timestamp — assume it's valid
		return true
	}
	if !campaignStart.IsZero() && awarded.Before(campaignStart) {
		return false // claimed before this campaign started
	}
	if !campaignEnd.IsZero() && awarded.After(campaignEnd) {
		return false // claimed after this campaign ended
	}
	return true
}

// getDropsDashboard fetches all campaigns via ViewerDropsDashboard.
func (g *GQLClient) getDropsDashboard() ([]DropCampaign, error) {
	req := &GQLRequest{
		OperationName: "ViewerDropsDashboard",
		Query:         queryDropsDashboard,
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get drops dashboard: %w", err)
	}

	currentUser, ok := resp.Data["currentUser"]
	if !ok || currentUser == nil {
		return nil, nil
	}
	userMap, ok := currentUser.(map[string]interface{})
	if !ok {
		return nil, nil
	}

	campaignsRaw, ok := userMap["dropCampaigns"]
	if !ok || campaignsRaw == nil {
		return nil, nil
	}
	campaignList, ok := campaignsRaw.([]interface{})
	if !ok {
		return nil, nil
	}

	return parseCampaignList(campaignList), nil
}

// getDropsFromInventory fetches campaigns via the Inventory query (fallback).
// Also returns gameEventDrops: a map of benefitID → lastAwardedAt for ALL ever-claimed benefits.
func (g *GQLClient) getDropsFromInventory() ([]DropCampaign, map[string]time.Time, error) {
	req := &GQLRequest{
		OperationName: "Inventory",
		Query:         queryDropsInventory,
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
	for _, cRaw := range campaignList {
		cMap, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}

		campaign := DropCampaign{
			ID:                 getString(cMap, "id"),
			Name:               getString(cMap, "name"),
			Status:             getString(cMap, "status"),
			IsAccountConnected: true, // default true — only false when API explicitly says so
		}

		// Parse game
		if game, ok := cMap["game"]; ok && game != nil {
			if gMap, ok := game.(map[string]interface{}); ok {
				campaign.GameID = getString(gMap, "id")
				campaign.GameName = getString(gMap, "displayName")
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

					// Parse benefit name
					if benefitEdges, ok := dMap["benefitEdges"]; ok && benefitEdges != nil {
						if edges, ok := benefitEdges.([]interface{}); ok && len(edges) > 0 {
							if edge, ok := edges[0].(map[string]interface{}); ok {
								if benefit, ok := edge["benefit"]; ok && benefit != nil {
									if bMap, ok := benefit.(map[string]interface{}); ok {
										drop.BenefitID = getString(bMap, "id")
									drop.BenefitName = getString(bMap, "name")
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
