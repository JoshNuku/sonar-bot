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
)

type NtfyService interface {
	NtfyStatus() (url string, lastStatus string, lastSentAt time.Time)
	TestNtfy() error
}

type Server struct {
	db            *database.DB
	adminPassword string
	hmacSecret    []byte
	deezer        *music.DeezerClient
	ntfy          NtfyService
}

func NewServer(db *database.DB, adminPassword, secretSalt string, dz *music.DeezerClient, ntfy NtfyService) *Server {
	if adminPassword == "" {
		adminPassword = "sonar2026"
	}
	hash := sha256.Sum256([]byte(adminPassword + ":" + secretSalt))
	return &Server{
		db:            db,
		adminPassword: adminPassword,
		hmacSecret:    hash[:],
		deezer:        dz,
		ntfy:          ntfy,
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
	mux.HandleFunc("/admin/api/ntfy/test", s.handleNtfyTest)
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

	rangeParam := r.URL.Query().Get("range")
	data, err := s.db.GetDashboardData(r.Context(), rangeParam)
	if err != nil {
		http.Error(w, fmt.Sprintf("database error: %v", err), http.StatusInternalServerError)
		return
	}

	ntfyURL, ntfyLastStatus, ntfyLastAt := "", "", time.Time{}
	if s.ntfy != nil {
		ntfyURL, ntfyLastStatus, ntfyLastAt = s.ntfy.NtfyStatus()
	}

	resp := map[string]interface{}{
		"range":            data.Range,
		"kpis":             data.KPIs,
		"chart_series":     data.ChartSeries,
		"users":            data.Users,
		"recent_downloads": data.RecentDownloads,
		"recent_payments":  data.RecentPayments,
		"format_stats":     data.FormatStats,
		"top_songs":        data.TopSongs,
		"recent_sessions":  data.RecentSessions,
		"ntfy": map[string]interface{}{
			"configured":  ntfyURL != "",
			"url":         ntfyURL,
			"last_status": ntfyLastStatus,
			"last_at":     ntfyLastAt,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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

func (s *Server) handleNtfyTest(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthenticated(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ntfy == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "ntfy service not configured"})
		return
	}
	if err := s.ntfy.TestNtfy(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	url, status, _ := s.ntfy.NtfyStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"url":    url,
		"status": status,
	})
}

