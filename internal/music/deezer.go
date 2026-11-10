package music

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Track struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Position int    `json:"position"`
	Rank     int    `json:"rank"`
	Duration int    `json:"duration"`
	Link     string `json:"link"`
}

type DeezerClient struct {
	client *http.Client
}

func NewDeezerClient() *DeezerClient {
	return &DeezerClient{
		client: &http.Client{
			Timeout: 4 * time.Second,
		},
	}
}

type deezerChartResponse struct {
	Data []struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Position int    `json:"position"`
		Rank     int    `json:"rank"`
		Duration int    `json:"duration"`
		Link     string `json:"link"`
		Artist   struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"data"`
}

type deezerSearchResponse struct {
	Data []struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Rank     int    `json:"rank"`
		Duration int    `json:"duration"`
		Artist   struct {
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"data"`
}

// GetTopTrending returns the top trending tracks from the global Deezer chart.
func (c *DeezerClient) GetTopTrending(ctx context.Context, limit int) ([]Track, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	apiURL := fmt.Sprintf("https://api.deezer.com/chart/0/tracks?limit=%d", limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SonarBot/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer chart error: status %d", resp.StatusCode)
	}

	var chart deezerChartResponse
	if err := json.NewDecoder(resp.Body).Decode(&chart); err != nil {
		return nil, err
	}

	var tracks []Track
	for _, item := range chart.Data {
		title := strings.TrimSpace(item.Title)
		artist := strings.TrimSpace(item.Artist.Name)
		if title == "" || artist == "" {
			continue
		}
		tracks = append(tracks, Track{
			ID:       item.ID,
			Title:    title,
			Artist:   artist,
			Position: item.Position,
			Rank:     item.Rank,
			Duration: item.Duration,
			Link:     item.Link,
		})
	}
	return tracks, nil
}

// ValidateTrack queries Deezer to verify if a song exists and returns its canonical "Artist - Title".
func (c *DeezerClient) ValidateTrack(ctx context.Context, query string) (string, bool) {
	clean := strings.TrimSpace(query)
	if clean == "" {
		return "", false
	}

	searchURL := fmt.Sprintf("https://api.deezer.com/search?q=%s&limit=1", url.QueryEscape(clean))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return clean, false
	}
	req.Header.Set("User-Agent", "SonarBot/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return clean, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return clean, false
	}

	var res deezerSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || len(res.Data) == 0 {
		return clean, false
	}

	best := res.Data[0]
	artistName := strings.TrimSpace(best.Artist.Name)
	trackTitle := strings.TrimSpace(best.Title)
	if artistName == "" || trackTitle == "" {
		return clean, false
	}

	return fmt.Sprintf("%s - %s", artistName, trackTitle), true
}

// ValidateSuggestions checks up to 5 suggested song titles concurrently against Deezer.
// It replaces titles with their canonical artist and title, or discards unverifiable ones.
func (c *DeezerClient) ValidateSuggestions(ctx context.Context, suggestions []string) []string {
	if len(suggestions) == 0 {
		return nil
	}

	type result struct {
		index int
		track string
		valid bool
	}

	resChan := make(chan result, len(suggestions))
	var wg sync.WaitGroup

	for i, s := range suggestions {
		wg.Add(1)
		go func(idx int, raw string) {
			defer wg.Done()
			subCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
			defer cancel()

			canonical, ok := c.ValidateTrack(subCtx, raw)
			if ok {
				resChan <- result{index: idx, track: canonical, valid: true}
			} else {
				if strings.Contains(raw, " - ") {
					resChan <- result{index: idx, track: raw, valid: true}
				} else {
					resChan <- result{index: idx, track: raw, valid: false}
				}
			}
		}(i, s)
	}

	wg.Wait()
	close(resChan)

	ordered := make([]string, len(suggestions))
	validMap := make(map[int]bool)
	for r := range resChan {
		if r.valid {
			ordered[r.index] = r.track
			validMap[r.index] = true
		}
	}

	var out []string
	seen := make(map[string]bool)
	for i, t := range ordered {
		if validMap[i] && t != "" {
			lower := strings.ToLower(t)
			if !seen[lower] {
				seen[lower] = true
				out = append(out, t)
			}
		}
	}
	return out
}
