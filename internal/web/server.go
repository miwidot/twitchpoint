package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/miwi/twitchpoint/internal/farmer"
)

//go:embed static/*
var staticFiles embed.FS

// Server is the HTTP server for the web UI.
type Server struct {
	farmer *farmer.Farmer
	port   int
	mux    *http.ServeMux
}

// New creates a new web server.
func New(f *farmer.Farmer, port int) *Server {
	s := &Server{
		farmer: f,
		port:   port,
		mux:    http.NewServeMux(),
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	// API routes
	s.mux.HandleFunc("/api/stats", s.handleStats)
	s.mux.HandleFunc("/api/channels", s.handleChannels)
	s.mux.HandleFunc("/api/channels/", s.handleChannel)
	s.mux.HandleFunc("/api/logs", s.handleLogs)

	// Static files (embedded)
	staticFS, _ := fs.Sub(staticFiles, "static")
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))
}

// Start starts the HTTP server (blocking).
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	return http.ListenAndServe(addr, s.mux)
}

// jsonResponse writes a JSON response with proper headers.
func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// jsonError writes a JSON error response.
func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// StatsResponse is the /api/stats response.
type StatsResponse struct {
	User             string `json:"user"`
	UserID           string `json:"user_id"`
	Uptime           string `json:"uptime"`
	TotalPoints      int    `json:"total_points"`
	TotalClaims      int    `json:"total_claims"`
	ChannelsOnline   int    `json:"channels_online"`
	ChannelsWatching int    `json:"channels_watching"`
	ChannelsTotal    int    `json:"channels_total"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.farmer.GetStats()
	user := s.farmer.GetUser()

	resp := StatsResponse{
		User:             user.DisplayName,
		UserID:           user.ID,
		Uptime:           formatDuration(stats.Uptime),
		TotalPoints:      stats.TotalPointsEarned,
		TotalClaims:      stats.TotalClaimsMade,
		ChannelsOnline:   stats.ChannelsOnline,
		ChannelsWatching: stats.ChannelsWatching,
		ChannelsTotal:    stats.ChannelsTotal,
	}

	jsonResponse(w, resp)
}

// ChannelResponse is a channel in the /api/channels response.
type ChannelResponse struct {
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	ChannelID   string `json:"channel_id"`
	Priority    int    `json:"priority"`
	IsOnline    bool   `json:"is_online"`
	IsWatching  bool   `json:"is_watching"`
	GameName    string `json:"game_name"`
	ViewerCount int    `json:"viewer_count"`
	Balance     int    `json:"balance"`
	Earned      int    `json:"earned"`
	Claims      int    `json:"claims"`
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		channels := s.farmer.GetChannels()
		resp := make([]ChannelResponse, len(channels))
		for i, ch := range channels {
			resp[i] = ChannelResponse{
				Login:       ch.Login,
				DisplayName: ch.DisplayName,
				ChannelID:   ch.ChannelID,
				Priority:    ch.Priority,
				IsOnline:    ch.IsOnline,
				IsWatching:  ch.IsWatching,
				GameName:    ch.GameName,
				ViewerCount: ch.ViewerCount,
				Balance:     ch.PointsBalance,
				Earned:      ch.PointsEarnedSession,
				Claims:      ch.ClaimsMade,
			}
		}
		jsonResponse(w, resp)

	case http.MethodPost:
		var req struct {
			Login string `json:"login"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Login == "" {
			jsonError(w, "login is required", http.StatusBadRequest)
			return
		}
		if err := s.farmer.AddChannelLive(req.Login); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		jsonResponse(w, map[string]string{"status": "ok", "login": req.Login})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	// Extract login from path: /api/channels/{login} or /api/channels/{login}/priority
	path := strings.TrimPrefix(r.URL.Path, "/api/channels/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		jsonError(w, "channel login required", http.StatusBadRequest)
		return
	}
	login := parts[0]

	// Check for /priority suffix
	if len(parts) >= 2 && parts[1] == "priority" {
		s.handleChannelPriority(w, r, login)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if err := s.farmer.RemoveChannelLive(login); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]string{"status": "ok", "login": login})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleChannelPriority(w http.ResponseWriter, r *http.Request, login string) {
	if r.Method != http.MethodPut {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Priority int `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Priority != 1 && req.Priority != 2 {
		jsonError(w, "priority must be 1 or 2", http.StatusBadRequest)
		return
	}

	if err := s.farmer.SetPriorityLive(login, req.Priority); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok", "login": login, "priority": fmt.Sprintf("%d", req.Priority)})
}

// LogResponse is a log entry in the /api/logs response.
type LogResponse struct {
	Time    string `json:"time"`
	Message string `json:"message"`
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logs := s.farmer.GetLogs()

	// Return last 50 entries (newest first)
	start := 0
	if len(logs) > 50 {
		start = len(logs) - 50
	}

	resp := make([]LogResponse, 0, 50)
	for i := len(logs) - 1; i >= start; i-- {
		resp = append(resp, LogResponse{
			Time:    logs[i].Time.Format("15:04:05"),
			Message: logs[i].Message,
		})
	}

	jsonResponse(w, resp)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}
