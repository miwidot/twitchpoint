package twitch

import (
	"fmt"
	"time"
)

// GQL query for fetching the user's drops inventory (active campaigns + progress).
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
	ID       string
	Name     string
	GameName string
	GameID   string
	StartAt  time.Time
	EndAt    time.Time
	Drops    []TimeBasedDrop
	Channels []DropChannel // allowed channels (empty = any channel with the game)
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

// GetDropsInventory fetches the user's active drop campaigns with progress.
func (g *GQLClient) GetDropsInventory() ([]DropCampaign, error) {
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

	var campaigns []DropCampaign
	for _, cRaw := range campaignList {
		cMap, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}

		campaign := DropCampaign{
			ID:   getString(cMap, "id"),
			Name: getString(cMap, "name"),
		}

		// Parse game
		if game, ok := cMap["game"]; ok && game != nil {
			if gMap, ok := game.(map[string]interface{}); ok {
				campaign.GameID = getString(gMap, "id")
				campaign.GameName = getString(gMap, "displayName")
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
						ID:                     getString(dMap, "id"),
						Name:                   getString(dMap, "name"),
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

	return campaigns, nil
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
