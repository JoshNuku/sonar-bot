package database

import (
	"context"
	"log"
	"time"
)

type DashboardKPIs struct {
	TotalUsers         int `json:"total_users"`
	ActiveToday        int `json:"active_today"`
	DownloadsToday     int `json:"downloads_today"`
	TotalCachedTracks  int `json:"total_cached_tracks"`
	TotalRevenueCents  int `json:"total_revenue_cents"`
	ActivePassesCount  int `json:"active_passes_count"`
	PeriodActiveUsers  int `json:"period_active_users"`
	PeriodDownloads    int `json:"period_downloads"`
	PeriodSearches     int `json:"period_searches"`
	PeriodRevenueCents int `json:"period_revenue_cents"`
	PeriodNewCached    int `json:"period_new_cached"`
}

type ChartPoint struct {
	Label        string `json:"label"`
	Downloads    int    `json:"downloads"`
	Searches     int    `json:"searches"`
	Users        int    `json:"users"`
	RevenueCents int    `json:"revenue_cents"`
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
	Range           string           `json:"range"`
	KPIs            DashboardKPIs    `json:"kpis"`
	ChartSeries     []ChartPoint     `json:"chart_series"`
	Users           []UserRecord     `json:"users"`
	RecentDownloads []DownloadRecord `json:"recent_downloads"`
	RecentPayments  []PaymentRecord  `json:"recent_payments"`
	FormatStats     []FormatCount    `json:"format_stats"`
	TopSongs        []TopSong        `json:"top_songs"`
	RecentSessions  []SessionRecord  `json:"recent_sessions"`
}

// GetDashboardData aggregates users, downloads, payments, and time-series stats for the admin UI.
func (db *DB) GetDashboardData(ctx context.Context, rangeParam string) (*DashboardData, error) {
	switch rangeParam {
	case "today", "week", "month", "all":
	default:
		rangeParam = "week"
	}

	data := &DashboardData{
		Range:           rangeParam,
		ChartSeries:     []ChartPoint{},
		Users:           []UserRecord{},
		RecentDownloads: []DownloadRecord{},
		RecentPayments:  []PaymentRecord{},
		FormatStats:     []FormatCount{},
		TopSongs:        []TopSong{},
		RecentSessions:  []SessionRecord{},
	}

	// 1. KPIs (global + period-scoped)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&data.KPIs.TotalUsers)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE last_reset_date >= CURRENT_DATE`).Scan(&data.KPIs.ActiveToday)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(daily_downloads), 0) FROM users WHERE last_reset_date >= CURRENT_DATE`).Scan(&data.KPIs.DownloadsToday)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM media_cache`).Scan(&data.KPIs.TotalCachedTracks)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents), 0) FROM processed_payments`).Scan(&data.KPIs.TotalRevenueCents)
	_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE is_premium = true OR (premium_until IS NOT NULL AND premium_until > NOW())`).Scan(&data.KPIs.ActivePassesCount)

	if rangeParam == "all" {
		data.KPIs.PeriodActiveUsers = data.KPIs.TotalUsers
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE session_type = 'download'`).Scan(&data.KPIs.PeriodDownloads)
		if data.KPIs.TotalCachedTracks > data.KPIs.PeriodDownloads {
			data.KPIs.PeriodDownloads = data.KPIs.TotalCachedTracks
		}
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE session_type IN ('search', 'youtube')`).Scan(&data.KPIs.PeriodSearches)
		data.KPIs.PeriodRevenueCents = data.KPIs.TotalRevenueCents
		data.KPIs.PeriodNewCached = data.KPIs.TotalCachedTracks
	} else {
		var since time.Time
		now := time.Now().UTC()
		switch rangeParam {
		case "today":
			since = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		case "week":
			since = now.Add(-7 * 24 * time.Hour)
		case "month":
			since = now.Add(-30 * 24 * time.Hour)
		}
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE last_seen_at >= $1`, since).Scan(&data.KPIs.PeriodActiveUsers)
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE session_type = 'download' AND created_at >= $1`, since).Scan(&data.KPIs.PeriodDownloads)
		if rangeParam == "today" && data.KPIs.DownloadsToday > data.KPIs.PeriodDownloads {
			data.KPIs.PeriodDownloads = data.KPIs.DownloadsToday
		}
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE session_type IN ('search', 'youtube') AND created_at >= $1`, since).Scan(&data.KPIs.PeriodSearches)
		_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents), 0) FROM processed_payments WHERE processed_at >= $1`, since).Scan(&data.KPIs.PeriodRevenueCents)
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM media_cache WHERE created_at >= $1`, since).Scan(&data.KPIs.PeriodNewCached)
	}

	// 1b. Time-series chart data
	data.ChartSeries = db.queryChartSeries(ctx, rangeParam)

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

func (db *DB) queryChartSeries(ctx context.Context, rangeParam string) []ChartPoint {
	now := time.Now().UTC()
	var (
		trunc      string
		keyFmt     string
		labelFmt   string
		bucketKeys []string
		series     []ChartPoint
		startTS    time.Time
	)

	switch rangeParam {
	case "today":
		trunc = "hour"
		keyFmt = "2006-01-02 15"
		labelFmt = "15:00"
		endHour := now.Truncate(time.Hour)
		startTS = endHour.Add(-23 * time.Hour)
		for i := 0; i < 24; i++ {
			t := startTS.Add(time.Duration(i) * time.Hour)
			bucketKeys = append(bucketKeys, t.Format(keyFmt))
			series = append(series, ChartPoint{Label: t.Format(labelFmt)})
		}
	case "month":
		trunc = "day"
		keyFmt = "2006-01-02"
		labelFmt = "Jan 02"
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		startTS = today.AddDate(0, 0, -29)
		for i := 0; i < 30; i++ {
			t := startTS.AddDate(0, 0, i)
			bucketKeys = append(bucketKeys, t.Format(keyFmt))
			series = append(series, ChartPoint{Label: t.Format(labelFmt)})
		}
	case "all":
		trunc = "month"
		keyFmt = "2006-01"
		labelFmt = "Jan 06"
		thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		startTS = thisMonth.AddDate(0, -11, 0)
		for i := 0; i < 12; i++ {
			t := startTS.AddDate(0, i, 0)
			bucketKeys = append(bucketKeys, t.Format(keyFmt))
			series = append(series, ChartPoint{Label: t.Format(labelFmt)})
		}
	default: // "week"
		trunc = "day"
		keyFmt = "2006-01-02"
		labelFmt = "Jan 02"
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		startTS = today.AddDate(0, 0, -6)
		for i := 0; i < 7; i++ {
			t := startTS.AddDate(0, 0, i)
			bucketKeys = append(bucketKeys, t.Format(keyFmt))
			series = append(series, ChartPoint{Label: t.Format(labelFmt)})
		}
	}

	idxByKey := make(map[string]int, len(bucketKeys))
	for i, k := range bucketKeys {
		idxByKey[k] = i
	}

	sessQuery := `
		SELECT
			DATE_TRUNC($1, created_at AT TIME ZONE 'UTC') AS bucket,
			COUNT(*) FILTER (WHERE session_type = 'download') AS downloads,
			COUNT(*) FILTER (WHERE session_type IN ('search', 'youtube')) AS searches,
			COUNT(DISTINCT chat_id) AS users
		FROM user_sessions
		WHERE created_at >= $2
		GROUP BY 1
	`
	sRows, err := db.Pool.Query(ctx, sessQuery, trunc, startTS)
	if err != nil {
		log.Printf("queryChartSeries sessions error: %v", err)
	} else {
		defer sRows.Close()
		for sRows.Next() {
			var bucket time.Time
			var downloads, searches, users int
			if scanErr := sRows.Scan(&bucket, &downloads, &searches, &users); scanErr == nil {
				if idx, ok := idxByKey[bucket.Format(keyFmt)]; ok {
					series[idx].Downloads = downloads
					series[idx].Searches = searches
					series[idx].Users = users
				}
			}
		}
	}

	payQuery := `
		SELECT
			DATE_TRUNC($1, processed_at AT TIME ZONE 'UTC') AS bucket,
			COALESCE(SUM(amount_cents), 0) AS revenue_cents
		FROM processed_payments
		WHERE processed_at >= $2
		GROUP BY 1
	`
	pRows, err := db.Pool.Query(ctx, payQuery, trunc, startTS)
	if err != nil {
		log.Printf("queryChartSeries payments error: %v", err)
	} else {
		defer pRows.Close()
		for pRows.Next() {
			var bucket time.Time
			var rev int
			if scanErr := pRows.Scan(&bucket, &rev); scanErr == nil {
				if idx, ok := idxByKey[bucket.Format(keyFmt)]; ok {
					series[idx].RevenueCents = rev
				}
			}
		}
	}

	return series
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

