package twitch

import (
	"fmt"
	"time"
)

// GQL query for fetching ALL available drop campaigns (not just ones with progress).
// Uses currentUser.dropCampaigns which is the Viewer Drops Dashboard — returns every
// campaign the user is eligible for, including ones they haven't started watching yet.
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

// GQL query for fetching progress on campaigns already started (fallback).
const queryDropsInventory = `query Inventory {
	currentUser {
		inventory {
			dropCampaignsInProgress {
				id
				name
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

// GetDropsInventory fetches ALL available drop campaigns using the ViewerDropsDashboard query.
// This returns every campaign the user is eligible for, not just ones with existing progress.
// Falls back to the old Inventory query if the dashboard query fails.
func (g *GQLClient) GetDropsInventory() ([]DropCampaign, error) {
	campaigns, err := g.getDropsDashboard()
	if err != nil || campaigns == nil {
		// Fallback to old inventory query
		return g.getDropsFromInventory()
	}
	return campaigns, nil
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

// getDropsFromInventory fetches campaigns via the old Inventory query (fallback).
func (g *GQLClient) getDropsFromInventory() ([]DropCampaign, error) {
	req := &GQLRequest{
		OperationName: "Inventory",
		Query:         queryDropsInventory,
	}

	resp, err := g.do(req)
	if err != nil {
		return nil, fmt.Errorf("get drops inventory: %w", err)
	}

	currentUser, ok := resp.Data["currentUser"]
	if !ok || currentUser == nil {
		return nil, nil
	}
	userMap, ok := currentUser.(map[string]interface{})
	if !ok {
		return nil, nil
	}

	inventory, ok := userMap["inventory"]
	if !ok || inventory == nil {
		return nil, nil
	}
	invMap, ok := inventory.(map[string]interface{})
	if !ok {
		return nil, nil
	}

	campaignsRaw, ok := invMap["dropCampaignsInProgress"]
	if !ok || campaignsRaw == nil {
		return nil, nil
	}
	campaignList, ok := campaignsRaw.([]interface{})
	if !ok {
		return nil, nil
	}

	return parseCampaignList(campaignList), nil
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
