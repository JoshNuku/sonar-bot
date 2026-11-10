package database

import (
	"context"
	"time"
)

type DashboardKPIs struct {
	TotalUsers        int `json:"total_users"`
	ActiveToday       int `json:"active_today"`
	DownloadsToday    int `json:"downloads_today"`
	TotalCachedTracks int `json:"total_cached_tracks"`
	TotalRevenueCents int `json:"total_revenue_cents"`
	ActivePassesCount int `json:"active_passes_count"`
}

type UserRecord struct {
	ChatID         int64      `json:"chat_id"`
	Username       string     `json:"username"`
	DailyDownloads int        `json:"daily_downloads"`
	LastResetDate  string     `json:"last_reset_date"`
	IsPremium      bool       `json:"is_premium"`
	PremiumUntil   *time.Time `json:"premium_until"`
	RecentSongs    []string   `json:"recent_songs"`
	LastLogin      time.Time  `json:"last_login"`
}

type DownloadRecord struct {
	YouTubeID   string    `json:"youtube_id"`
	MediaFormat string    `json:"media_format"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
}

type PaymentRecord struct {
	Reference   string    `json:"reference"`
	ChatID      int64     `json:"chat_id"`
	AmountCents int       `json:"amount_cents"`
	ProcessedAt time.Time `json:"processed_at"`
	PlanTier    string    `json:"plan_tier"`
}

type FormatCount struct {
	Format string `json:"format"`
	Count  int    `json:"count"`
}

type TopSong struct {
	Title string `json:"title"`
	Count int    `json:"count"`
}

type SessionRecord struct {
	ID          int64     `json:"id"`
	ChatID      int64     `json:"chat_id"`
	Username    string    `json:"username"`
	SessionType string    `json:"session_type"`
	Details     string    `json:"details"`
	CreatedAt   time.Time `json:"created_at"`
}

type DashboardData struct {
	KPIs            DashboardKPIs    `json:"kpis"`
	Users           []UserRecord     `json:"users"`
	RecentDownloads []DownloadRecord `json:"recent_downloads"`
	RecentPayments  []PaymentRecord  `json:"recent_payments"`
	FormatStats     []FormatCount    `json:"format_stats"`
	TopSongs        []TopSong        `json:"top_songs"`
	RecentSessions  []SessionRecord  `json:"recent_sessions"`
}

// GetDashboardData aggregates users, downloads, payments, and stats for the admin UI.
func (db *DB) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	data := &DashboardData{
		Users:           []UserRecord{},
		RecentDownloads: []DownloadRecord{},
		RecentPayments:  []PaymentRecord{},
		FormatStats:     []FormatCount{},
		TopSongs:        []TopSong{},
		RecentSessions:  []SessionRecord{},
	}

	// 1. KPIs
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&data.KPIs.TotalUsers)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE last_reset_date >= CURRENT_DATE`).Scan(&data.KPIs.ActiveToday)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(daily_downloads), 0) FROM users WHERE last_reset_date >= CURRENT_DATE`).Scan(&data.KPIs.DownloadsToday)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM media_cache`).Scan(&data.KPIs.TotalCachedTracks)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents), 0) FROM processed_payments`).Scan(&data.KPIs.TotalRevenueCents)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE is_premium = true OR (premium_until IS NOT NULL AND premium_until > NOW())`).Scan(&data.KPIs.ActivePassesCount)

	// 2. Users (latest 100 sorted by last login / activity)
	uRows, err := db.Pool.Query(ctx, `
		SELECT 
			u.chat_id, 
			COALESCE(u.username, ''), 
			u.daily_downloads, 
			u.last_reset_date, 
			u.is_premium, 
			u.premium_until, 
			COALESCE(u.recent_songs, '{}'),
			COALESCE(u.last_seen_at, (SELECT MAX(s.created_at) FROM user_sessions s WHERE s.chat_id = u.chat_id), u.last_reset_date, NOW()) AS last_login
		FROM users u
		ORDER BY last_login DESC NULLS LAST, u.chat_id DESC
		LIMIT 100
	`)
	if err == nil {
		defer uRows.Close()
		for uRows.Next() {
			var u UserRecord
			var resetDate time.Time
			if err := uRows.Scan(&u.ChatID, &u.Username, &u.DailyDownloads, &resetDate, &u.IsPremium, &u.PremiumUntil, &u.RecentSongs, &u.LastLogin); err == nil {
				u.LastResetDate = resetDate.Format("2006-01-02")
				data.Users = append(data.Users, u)
			}
		}
	}

	// 3. Recent Downloads (latest 50)
	dRows, err := db.Pool.Query(ctx, `
		SELECT youtube_id, media_format, title, created_at
		FROM media_cache
		ORDER BY created_at DESC
		LIMIT 50
	`)
	if err == nil {
		defer dRows.Close()
		for dRows.Next() {
			var d DownloadRecord
			if err := dRows.Scan(&d.YouTubeID, &d.MediaFormat, &d.Title, &d.CreatedAt); err == nil {
				data.RecentDownloads = append(data.RecentDownloads, d)
			}
		}
	}

	// 4. Recent Payments (latest 50)
	pRows, err := db.Pool.Query(ctx, `
		SELECT reference, chat_id, amount_cents, processed_at, COALESCE(plan_tier, '')
		FROM processed_payments
		ORDER BY processed_at DESC
		LIMIT 50
	`)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var p PaymentRecord
			if err := pRows.Scan(&p.Reference, &p.ChatID, &p.AmountCents, &p.ProcessedAt, &p.PlanTier); err == nil {
				data.RecentPayments = append(data.RecentPayments, p)
			}
		}
	}

	// 5. Format Breakdown
	fRows, err := db.Pool.Query(ctx, `
		SELECT media_format, COUNT(*)
		FROM media_cache
		GROUP BY media_format
	`)
	if err == nil {
		defer fRows.Close()
		for fRows.Next() {
			var fc FormatCount
			if err := fRows.Scan(&fc.Format, &fc.Count); err == nil {
				data.FormatStats = append(data.FormatStats, fc)
			}
		}
	}

	// 6. Top Songs
	tRows, err := db.Pool.Query(ctx, `
		SELECT title, COUNT(*) as cnt
		FROM media_cache
		GROUP BY title
		ORDER BY cnt DESC
		LIMIT 8
	`)
	if err == nil {
		defer tRows.Close()
		for tRows.Next() {
			var ts TopSong
			if err := tRows.Scan(&ts.Title, &ts.Count); err == nil {
				data.TopSongs = append(data.TopSongs, ts)
			}
		}
	}

	// 7. Recent Sessions (latest 100)
	sRows, err := db.Pool.Query(ctx, `
		SELECT id, chat_id, COALESCE(username, ''), session_type, details, created_at
		FROM user_sessions
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err == nil {
		defer sRows.Close()
		for sRows.Next() {
			var s SessionRecord
			if err := sRows.Scan(&s.ID, &s.ChatID, &s.Username, &s.SessionType, &s.Details, &s.CreatedAt); err == nil {
				data.RecentSessions = append(data.RecentSessions, s)
			}
		}
	}

	return data, nil
}

// GrantManualPass allows an admin to immediately grant a user premium access for a specific number of hours.
func (db *DB) GrantManualPass(ctx context.Context, chatID int64, hours int) error {
	if hours <= 0 {
		hours = 24
	}
	expiresAt := time.Now().UTC().Add(time.Duration(hours) * time.Hour)
	query := `
		INSERT INTO users (chat_id, username, daily_downloads, last_reset_date, is_premium, premium_until, recent_songs)
		VALUES ($1, '', 0, CURRENT_DATE, true, $2, '{}')
		ON CONFLICT (chat_id) DO UPDATE
		SET is_premium = true, premium_until = $2;
	`
	_, err := db.Pool.Exec(ctx, query, chatID, expiresAt)
	return err
}

