package twitter

import (
	"context"
	"log"
	"time"

	"sonar/internal/database"
	"sonar/internal/music"
)

type Scheduler struct {
	client *Client
	db     *database.DB
	deezer *music.DeezerClient
}

func NewScheduler(client *Client, db *database.DB, deezer *music.DeezerClient) *Scheduler {
	return &Scheduler{
		client: client,
		db:     db,
		deezer: deezer,
	}
}

// Start launches a background loop that periodically tweets music drops.
func (s *Scheduler) Start() {
	if !s.client.IsConfigured() {
		log.Printf("[Twitter Scheduler] Twitter credentials not set. Automated posting idle.")
		return
	}

	log.Printf("[Twitter Scheduler] Automated Twitter bot active. Posting daily music drops.")

	go func() {
		// Run initial check after 2 minutes of bot startup
		time.Sleep(2 * time.Minute)
		s.runScheduledDrop()

		// Then check every 12 hours
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			s.runScheduledDrop()
		}
	}()
}

func (s *Scheduler) runScheduledDrop() {
	if !s.client.IsConfigured() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Alternate between Trending Tracks and Sonar Top Downloads based on current hour
	nowHour := time.Now().UTC().Hour()
	if nowHour >= 6 && nowHour < 16 {
		// Morning drop: Global Trending tracks
		tracks, err := s.deezer.GetTopTrending(ctx, 5)
		if err == nil && len(tracks) > 0 {
			if err := s.client.PostTrendingDrop(ctx, tracks); err != nil {
				log.Printf("[Twitter Scheduler] Failed to tweet trending drop: %v", err)
			}
			return
		}
	}

	// Evening drop: Sonar community most downloaded
	dashData, err := s.db.GetDashboardData(ctx)
	if err == nil && len(dashData.TopSongs) > 0 {
		if err := s.client.PostTopDownloaded(ctx, dashData.TopSongs); err != nil {
			log.Printf("[Twitter Scheduler] Failed to tweet top downloads: %v", err)
		}
	}
}
