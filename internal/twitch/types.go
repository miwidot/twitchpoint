package twitch

import "time"

// PubSub message types
const (
	PubSubTypePing     = "PING"
	PubSubTypePong     = "PONG"
	PubSubTypeListen   = "LISTEN"
	PubSubTypeUnlisten = "UNLISTEN"
	PubSubTypeMessage  = "MESSAGE"
	PubSubTypeResponse = "RESPONSE"
	PubSubTypeReconn   = "RECONNECT"
)

// PubSubOutgoing is a message sent to PubSub.
type PubSubOutgoing struct {
	Type  string         `json:"type"`
	Nonce string         `json:"nonce,omitempty"`
	Data  *PubSubListen  `json:"data,omitempty"`
}

// PubSubListen is the data payload for LISTEN/UNLISTEN.
type PubSubListen struct {
	Topics    []string `json:"topics"`
	AuthToken string   `json:"auth_token,omitempty"`
}

// PubSubIncoming is a message received from PubSub.
type PubSubIncoming struct {
	Type  string          `json:"type"`
	Nonce string          `json:"nonce,omitempty"`
	Error string          `json:"error,omitempty"`
	Data  *PubSubMsgData  `json:"data,omitempty"`
}

// PubSubMsgData is the data field of a MESSAGE type.
type PubSubMsgData struct {
	Topic   string `json:"topic"`
	Message string `json:"message"` // JSON string, needs second parse
}

// Community points claim event (inner message)
type PointsClaimAvailable struct {
	Type string `json:"type"` // "claim-available"
	Data struct {
		Claim struct {
			ID         string `json:"id"`
			ChannelID  string `json:"channel_id"`
			PointsEarned int  `json:"points_earned"`
		} `json:"claim"`
	} `json:"data"`
}

// Community points earned event
type PointsEarned struct {
	Type string `json:"type"` // "points-earned"
	Data struct {
		Timestamp    time.Time `json:"timestamp"`
		ChannelID    string    `json:"channel_id"`
		PointGain    struct {
			UserID       string `json:"user_id"`
			ChannelID    string `json:"channel_id"`
			TotalPoints  int    `json:"total_points"`
			BaselineGain int    `json:"baseline_gain"`
			ReasonCode   string `json:"reason_code"`
		} `json:"point_gain"`
		Balance struct {
			ChannelID    string `json:"channel_id"`
			CurrentPoints int   `json:"current_points"`
		} `json:"balance"`
	} `json:"data"`
}

// Community points claim response wrapper
type CommunityPointsEvent struct {
	Type string `json:"type"`
	Data struct {
		Timestamp    string `json:"timestamp"`
		ChannelID    string `json:"channel_id"`
		Claim        *struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		} `json:"claim,omitempty"`
		PointGain    *struct {
			UserID       string `json:"user_id"`
			ChannelID    string `json:"channel_id"`
			TotalPoints  int    `json:"total_points"`
			BaselineGain int    `json:"baseline_gain"`
			ReasonCode   string `json:"reason_code"`
		} `json:"point_gain,omitempty"`
		Balance      *struct {
			ChannelID     string `json:"channel_id"`
			Balance       int    `json:"balance"`
		} `json:"balance,omitempty"`
	} `json:"data"`
}

// Video playback event (stream up/down)
type VideoPlaybackEvent struct {
	Type       string `json:"type"` // "stream-up", "stream-down", "viewcount"
	ServerTime float64 `json:"server_time,omitempty"`
	PlayDelay  int     `json:"play_delay,omitempty"`
	Viewers    int     `json:"viewers,omitempty"`
}

// Raid event
type RaidEvent struct {
	Type string `json:"type"` // "raid_update_v2"
	Raid struct {
		ID                string `json:"id"`
		CreatorID         string `json:"creator_id"`
		SourceID          string `json:"source_id"`
		TargetID          string `json:"target_id"`
		TargetLogin       string `json:"target_login"`
		TargetDisplayName string `json:"target_display_name"`
		ViewerCount       int    `json:"viewer_count"`
	} `json:"raid"`
}

// GQL request/response types
type GQLRequest struct {
	OperationName string                 `json:"operationName"`
	Variables     map[string]interface{} `json:"variables,omitempty"`
	Extensions    *GQLExtensions         `json:"extensions,omitempty"`
	Query         string                 `json:"query,omitempty"`
}

type GQLExtensions struct {
	PersistedQuery *PersistedQuery `json:"persistedQuery"`
}

type PersistedQuery struct {
	Version    int    `json:"version"`
	SHA256Hash string `json:"sha256Hash"`
}

type GQLResponse struct {
	Data   map[string]interface{} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// User info from GQL
type UserInfo struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
}

// Channel info
type ChannelInfo struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
	IsLive      bool
	BroadcastID string
	GameName    string
	ViewerCount int
}

// Stream metadata
type StreamInfo struct {
	BroadcastID string
	GameName    string
	ViewerCount int
	StartedAt   time.Time
}

// FarmerEvent is an event emitted to the farmer from various subsystems.
type FarmerEvent struct {
	Type      FarmerEventType
	ChannelID string
	Data      interface{}
}

type FarmerEventType int

const (
	EventClaimAvailable FarmerEventType = iota
	EventPointsEarned
	EventStreamUp
	EventStreamDown
	EventRaid
	EventViewCount
	EventError
)

// ClaimData holds data for a claim-available event.
type ClaimData struct {
	ClaimID string
}

// PointsData holds data for a points-earned event.
type PointsData struct {
	PointsGained  int
	TotalPoints   int
	ReasonCode    string
}

// RaidData holds data for a raid event.
type RaidData struct {
	RaidID            string
	TargetLogin       string
	TargetDisplayName string
}

// ViewCountData holds data for a viewcount event.
type ViewCountData struct {
	Viewers int
}
