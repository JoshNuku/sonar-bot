package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sonar/internal/database"
	"sonar/internal/music"
	"sonar/internal/twitter"
)

type Server struct {
	db            *database.DB
	adminPassword string
	hmacSecret    []byte
	twitter       *twitter.Client
	deezer        *music.DeezerClient
}

func NewServer(db *database.DB, adminPassword, secretSalt string, tw *twitter.Client, dz *music.DeezerClient) *Server {
	if adminPassword == "" {
		adminPassword = "sonar2026"
	}
	hash := sha256.Sum256([]byte(adminPassword + ":" + secretSalt))
	return &Server{
		db:            db,
		adminPassword: adminPassword,
		hmacSecret:    hash[:],
		twitter:       tw,
		deezer:        dz,
	}
}

func (s *Server) generateSessionToken() string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, s.hmacSecret)
	mac.Write([]byte(ts))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s", ts, sig)
}

func (s *Server) validateSessionToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	tsStr, sig := parts[0], parts[1]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}

	// 7 days expiration
	if time.Since(time.Unix(ts, 0)) > 7*24*time.Hour {
		return false
	}

	mac := hmac.New(sha256.New, s.hmacSecret)
	mac.Write([]byte(tsStr))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expectedSig))
}

func (s *Server) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie("sonar_admin_session")
	if err != nil || cookie == nil {
		return false
	}
	return s.validateSessionToken(cookie.Value)
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin", s.handleDashboardPage)
	mux.HandleFunc("/admin/", s.handleDashboardPage)
	mux.HandleFunc("/admin/api/login", s.handleLogin)
	mux.HandleFunc("/admin/api/logout", s.handleLogout)
	mux.HandleFunc("/admin/api/data", s.handleData)
	mux.HandleFunc("/admin/api/grant-pass", s.handleGrantPass)
	mux.HandleFunc("/admin/api/twitter/tweet", s.handleTwitterTweet)
	mux.HandleFunc("/admin/api/twitter/trending", s.handleTwitterTrending)
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if req.Password != s.adminPassword {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": "Invalid admin password",
		})
		return
	}

	token := s.generateSessionToken()
	http.SetCookie(w, &http.Cookie{
		Name:     "sonar_admin_session",
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(7 * 24 * time.Hour.Seconds()),
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sonar_admin_session",
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthenticated(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	data, err := s.db.GetDashboardData(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("database error: %v", err), http.StatusInternalServerError)
		return
	}

	isTwitterConfigured := s.twitter != nil && s.twitter.IsConfigured()

	resp := map[string]interface{}{
		"kpis":               data.KPIs,
		"users":              data.Users,
		"recent_downloads":   data.RecentDownloads,
		"recent_payments":    data.RecentPayments,
		"format_stats":       data.FormatStats,
		"top_songs":          data.TopSongs,
		"recent_sessions":    data.RecentSessions,
		"twitter_configured": isTwitterConfigured,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTwitterTweet(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthenticated(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.twitter == nil || !s.twitter.IsConfigured() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": "Twitter API credentials are not set in .env",
		})
		return
	}

	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "Text is required", http.StatusBadRequest)
		return
	}

	if err := s.twitter.PostTweet(r.Context(), req.Text); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *Server) handleTwitterTrending(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthenticated(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.twitter == nil || !s.twitter.IsConfigured() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": "Twitter API credentials are not set in .env",
		})
		return
	}

	if s.deezer == nil {
		http.Error(w, "Music provider unavailable", http.StatusInternalServerError)
		return
	}

	tracks, err := s.deezer.GetTopTrending(r.Context(), 5)
	if err != nil || len(tracks) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": "Failed to fetch trending tracks",
		})
		return
	}

	if err := s.twitter.PostTrendingDrop(r.Context(), tracks); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *Server) handleGrantPass(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthenticated(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ChatID int64 `json:"chat_id"`
		Hours  int   `json:"hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatID == 0 {
		http.Error(w, "Invalid parameters", http.StatusBadRequest)
		return
	}

	if err := s.db.GrantManualPass(r.Context(), req.ChatID, req.Hours); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

