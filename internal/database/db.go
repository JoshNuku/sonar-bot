package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool           *pgxpool.Pool
	DailyFreeLimit int
}

func NewPool(connStr string, dailyFreeLimit int) (*DB, error) {
	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("invalid connection string: %w", err)
	}

	config.MaxConns = 15
	config.MinConns = 0
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 2 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("pool creation failed: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	if dailyFreeLimit <= 0 {
		dailyFreeLimit = 5
	}

	db := &DB{Pool: pool, DailyFreeLimit: dailyFreeLimit}
	if err := db.EnsureSchema(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema migration failed: %w", err)
	}

	return db, nil
}

// EnsureSchema safely adds time-based pass columns, personalized history, and cache constraints.
func (db *DB) EnsureSchema(ctx context.Context) error {
	queries := []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS premium_until TIMESTAMPTZ;`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS recent_songs TEXT[] DEFAULT '{}';`,
		`ALTER TABLE processed_payments ADD COLUMN IF NOT EXISTS plan_tier TEXT;`,
		`CREATE TABLE IF NOT EXISTS media_cache (
			youtube_id TEXT NOT NULL,
			media_format TEXT NOT NULL,
			tg_file_id TEXT NOT NULL,
			title TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);`,
		`ALTER TABLE media_cache DROP CONSTRAINT IF EXISTS media_cache_pkey;`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_media_cache_yt_fmt ON media_cache (youtube_id, media_format);`,
		`CREATE TABLE IF NOT EXISTS user_sessions (
			id BIGSERIAL PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			username TEXT DEFAULT '',
			session_type TEXT NOT NULL,
			details TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_user_sessions_created_at ON user_sessions (created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_user_sessions_chat_id ON user_sessions (chat_id);`,
	}
	for _, q := range queries {
		if _, err := db.Pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

func (db *DB) CheckQuota(ctx context.Context, chatID int64, username string) (bool, int, []string, error) {
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)

	var dailyDownloads int
	var lastResetDate time.Time
	var isPremium bool
	var premiumUntil *time.Time
	var recentSongs []string

	query := `SELECT daily_downloads, last_reset_date, is_premium, premium_until, COALESCE(recent_songs, '{}') FROM users WHERE chat_id = $1`
	err := db.Pool.QueryRow(ctx, query, chatID).Scan(&dailyDownloads, &lastResetDate, &isPremium, &premiumUntil, &recentSongs)

	if errors.Is(err, pgx.ErrNoRows) {
		insert := `INSERT INTO users (chat_id, username, daily_downloads, last_reset_date, is_premium, recent_songs)
		           VALUES ($1, $2, 0, $3, false, '{}')`
		_, err = db.Pool.Exec(ctx, insert, chatID, username, today)
		return db.DailyFreeLimit > 0, db.DailyFreeLimit, nil, err
	}
	if err != nil {
		return false, 0, nil, err
	}

	if lastResetDate.Before(today) {
		dailyDownloads = 0
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET daily_downloads = 0, last_reset_date = $1 WHERE chat_id = $2`, today, chatID)
	}

	// Check time-based pass expiration
	if premiumUntil != nil {
		if premiumUntil.After(now) {
			return true, 999, recentSongs, nil
		}
		if isPremium {
			isPremium = false
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_premium = false WHERE chat_id = $1`, chatID)
		}
	} else if isPremium {
		return true, 999, recentSongs, nil
	}

	if dailyDownloads < db.DailyFreeLimit {
		return true, db.DailyFreeLimit - dailyDownloads, recentSongs, nil
	}

	return false, 0, recentSongs, nil
}

// RecordUserSong prepends a song to the user's personalized history (deduplicated, max 15 recent songs).
func (db *DB) RecordUserSong(ctx context.Context, chatID int64, songTitle string) {
	songTitle = strings.TrimSpace(songTitle)
	if songTitle == "" || songTitle == "Sonar Track" {
		return
	}
	query := `
		UPDATE users
		SET recent_songs = (
			SELECT ARRAY(
				SELECT e
				FROM unnest(ARRAY[$1::text] || array_remove(COALESCE(recent_songs, '{}'), $1::text)) WITH ORDINALITY AS t(e, ord)
				ORDER BY ord
				LIMIT 15
			)
		)
		WHERE chat_id = $2`
	_, _ = db.Pool.Exec(ctx, query, songTitle, chatID)
}

func (db *DB) IncrementUsage(ctx context.Context, chatID int64) error {
	_, err := db.Pool.Exec(ctx, `UPDATE users SET daily_downloads = daily_downloads + 1 WHERE chat_id = $1`, chatID)
	return err
}

func (db *DB) GetCachedFileID(ctx context.Context, youtubeID, format string) (string, string, error) {
	var fileID, title string
	query := `SELECT tg_file_id, title FROM media_cache WHERE youtube_id = $1 AND media_format = $2`
	err := db.Pool.QueryRow(ctx, query, youtubeID, format).Scan(&fileID, &title)
	if err != nil {
		return "", "", err
	}
	return fileID, title, nil
}

func (db *DB) SaveCachedFileID(ctx context.Context, youtubeID, format, fileID, title string) error {
	query := `INSERT INTO media_cache (youtube_id, media_format, tg_file_id, title)
	          VALUES ($1, $2, $3, $4)
	          ON CONFLICT (youtube_id, media_format) DO UPDATE SET tg_file_id = EXCLUDED.tg_file_id, title = EXCLUDED.title`
	_, err := db.Pool.Exec(ctx, query, youtubeID, format, fileID, title)
	return err
}

func (db *DB) ProcessPaymentSuccess(ctx context.Context, ref string, chatID int64, amount int, planTier string, duration time.Duration) (time.Time, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)

	// Idempotent insertion
	_, err = tx.Exec(ctx,
		"INSERT INTO processed_payments (reference, chat_id, amount, plan_tier) VALUES ($1, $2, $3, $4)",
		ref, chatID, amount, planTier,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return time.Time{}, nil // Already processed
		}
		return time.Time{}, err
	}

	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE users
		SET is_premium = true,
		    premium_until = GREATEST(COALESCE(premium_until, NOW()), NOW()) + $1::interval
		WHERE chat_id = $2
		RETURNING premium_until
	`, fmt.Sprintf("%d seconds", int(duration.Seconds())), chatID).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, err
	}

	return expiresAt, nil
}

// RecordSession inserts an activity event into the user_sessions audit log.
func (db *DB) RecordSession(ctx context.Context, chatID int64, username, sessionType, details string) {
	if details == "" {
		return
	}
	if len(details) > 300 {
		details = details[:300]
	}
	query := `INSERT INTO user_sessions (chat_id, username, session_type, details) VALUES ($1, $2, $3, $4)`
	_, _ = db.Pool.Exec(ctx, query, chatID, username, sessionType, details)
}
