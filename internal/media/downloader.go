package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type TrackInfo struct {
	ID       string
	Title    string
	Duration string
	Channel  string
}

type Downloader struct {
	concurrencySem chan struct{}
	cmdName        string
	baseArgs       []string
	httpClient     *http.Client
	trackMeta      sync.Map // videoID -> TrackInfo
	inflightMu     sync.Mutex
	inflight       map[string]*inflightDownload
}

type inflightDownload struct {
	done chan struct{}
	path string
	err  error
}

func resolveYtDlpCommand() (string, []string) {
	// Prefer native Python module if installed (avoids 4-5s Windows PyInstaller .exe unpacking overhead)
	if pyPath, err := exec.LookPath("python"); err == nil {
		check := exec.Command(pyPath, "-c", "import yt_dlp")
		if check.Run() == nil {
			return pyPath, []string{"-m", "yt_dlp"}
		}
	}
	if path, err := exec.LookPath("yt-dlp"); err == nil {
		return path, nil
	}
	for _, candidate := range []string{"yt-dlp.exe", "yt-dlp"} {
		if abs, err := filepath.Abs(candidate); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				return abs, nil
			}
		}
	}
	return "yt-dlp", nil
}

func NewDownloader(maxParallel int) *Downloader {
	cmdName, baseArgs := resolveYtDlpCommand()
	return &Downloader{
		concurrencySem: make(chan struct{}, maxParallel),
		cmdName:        cmdName,
		baseArgs:       baseArgs,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		inflight: make(map[string]*inflightDownload),
	}
}

func (d *Downloader) GetCachedTrackInfo(videoID string) (TrackInfo, bool) {
	if val, ok := d.trackMeta.Load(videoID); ok {
		return val.(TrackInfo), true
	}
	return TrackInfo{}, false
}

func (d *Downloader) RememberTrack(info TrackInfo) {
	if info.ID != "" {
		d.trackMeta.Store(info.ID, info)
	}
}

// Typed structs for YouTube InnerTube search to strictly preserve result order.
type innerTubeSearchResponse struct {
	Contents struct {
		TwoColumnSearchResultsRenderer struct {
			PrimaryContents struct {
				SectionListRenderer struct {
					Contents []struct {
						ItemSectionRenderer struct {
							Contents []struct {
								VideoRenderer *struct {
									VideoID string `json:"videoId"`
									Title   struct {
										Runs []struct {
											Text string `json:"text"`
										} `json:"runs"`
										SimpleText string `json:"simpleText"`
									} `json:"title"`
									LengthText struct {
										SimpleText string `json:"simpleText"`
									} `json:"lengthText"`
									OwnerText struct {
										Runs []struct {
											Text string `json:"text"`
										} `json:"runs"`
									} `json:"ownerText"`
								} `json:"videoRenderer"`
							} `json:"contents"`
						} `json:"itemSectionRenderer"`
					} `json:"contents"`
				} `json:"sectionListRenderer"`
			} `json:"primaryContents"`
		} `json:"twoColumnSearchResultsRenderer"`
	} `json:"contents"`
}

func (d *Downloader) resolveTracksViaInnerTube(ctx context.Context, query string, maxResults int) ([]TrackInfo, error) {
	payload := map[string]interface{}{
		"context": map[string]interface{}{
			"client": map[string]interface{}{
				"clientName":    "WEB",
				"clientVersion": "2.20261101.01.00",
				"hl":            "en",
				"gl":            "US",
			},
		},
		"query": query,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://www.youtube.com/youtubei/v1/search?prettyPrint=false",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("innertube status %d", resp.StatusCode)
	}

	var parsed innerTubeSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var results []TrackInfo
	sections := parsed.Contents.TwoColumnSearchResultsRenderer.PrimaryContents.SectionListRenderer.Contents
	for _, sec := range sections {
		for _, item := range sec.ItemSectionRenderer.Contents {
			vr := item.VideoRenderer
			if vr == nil || vr.VideoID == "" {
				continue
			}

			title := vr.Title.SimpleText
			if title == "" {
				var sb strings.Builder
				for _, r := range vr.Title.Runs {
					sb.WriteString(r.Text)
				}
				title = sb.String()
			}
			title = strings.TrimSpace(title)
			if title == "" {
				continue
			}

			var channel string
			if len(vr.OwnerText.Runs) > 0 {
				channel = vr.OwnerText.Runs[0].Text
			}

			info := TrackInfo{
				ID:       vr.VideoID,
				Title:    title,
				Duration: vr.LengthText.SimpleText,
				Channel:  channel,
			}
			d.RememberTrack(info)
			results = append(results, info)
			if len(results) >= maxResults {
				return results, nil
			}
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no matching tracks in innertube")
	}
	return results, nil
}

func (d *Downloader) ResolveTracks(ctx context.Context, query string, maxResults int) ([]TrackInfo, error) {
	if maxResults <= 0 {
		maxResults = 5
	}

	// 1. Fastpath: Pure-Go InnerTube search (~150-250ms)
	if results, err := d.resolveTracksViaInnerTube(ctx, query, maxResults); err == nil && len(results) > 0 {
		return results, nil
	}

	// 2. Fallback: Fast flat-playlist yt-dlp search
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	args := append([]string{}, d.baseArgs...)
	args = append(args,
		"--flat-playlist",
		"--no-warnings",
		"--print", "%(id)s|||%(title)s|||%(duration_string)s",
		"--no-playlist",
		fmt.Sprintf("ytsearch%d:%s", maxResults, query),
	)

	cmd := exec.CommandContext(ctx, d.cmdName, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	var results []TrackInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "|||") {
			continue
		}
		parts := strings.Split(line, "|||")
		if len(parts) >= 2 && strings.TrimSpace(parts[0]) != "" {
			dur := ""
			if len(parts) >= 3 && parts[2] != "NA" {
				dur = strings.TrimSpace(parts[2])
			}
			info := TrackInfo{
				ID:       strings.TrimSpace(parts[0]),
				Title:    strings.TrimSpace(parts[1]),
				Duration: dur,
			}
			d.RememberTrack(info)
			results = append(results, info)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no matching tracks found")
	}
	return results, nil
}

// PrefetchAudio starts warming/downloading the audio stream in the background when a user selects a track.
func (d *Downloader) PrefetchAudio(videoID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = d.DownloadStream(ctx, videoID, "audio")
	}()
}

func (d *Downloader) DownloadStream(ctx context.Context, videoID, format string) (string, error) {
	key := fmt.Sprintf("%s:%s", format, videoID)

	// Deduplicate concurrent/prefetched downloads for the same videoID + format
	d.inflightMu.Lock()
	if existing, ok := d.inflight[key]; ok {
		d.inflightMu.Unlock()
		select {
		case <-existing.done:
			if existing.err == nil {
				if _, statErr := os.Stat(existing.path); statErr == nil {
					return existing.path, nil
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
		d.inflightMu.Lock()
	}

	call := &inflightDownload{done: make(chan struct{})}
	d.inflight[key] = call
	d.inflightMu.Unlock()

	defer func() {
		close(call.done)
		d.inflightMu.Lock()
		delete(d.inflight, key)
		d.inflightMu.Unlock()
	}()

	// Fastpath if prefetched file already exists on disk
	globPattern := filepath.Join(os.TempDir(), fmt.Sprintf("sonar_%s_%s.*", format, videoID))
	if matches, err := filepath.Glob(globPattern); err == nil {
		for _, m := range matches {
			if !strings.HasSuffix(m, ".part") && !strings.HasSuffix(m, ".ytdl") {
				if info, statErr := os.Stat(m); statErr == nil && info.Size() > 0 {
					call.path = m
					return m, nil
				}
			}
		}
	}

	select {
	case d.concurrencySem <- struct{}{}:
		defer func() { <-d.concurrencySem }()
	case <-ctx.Done():
		call.err = ctx.Err()
		return "", ctx.Err()
	}

	outTemplate := filepath.Join(os.TempDir(), fmt.Sprintf("sonar_%s_%%(id)s.%%(ext)s", format))
	videoURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)

	args := append([]string{}, d.baseArgs...)
	if format == "audio" {
		// Strictly select high-bitrate 128-160kbps 44.1kHz stereo AAC (format 140 / ba[ext=m4a]), never 48kbps format 139
		args = append(args,
			"--js-runtimes", "node",
			"--no-warnings",
			"--concurrent-fragments", "16",
			"--match-filter", "duration <= 900",
			"-f", "140/ba[abr>=120][ext=m4a]/ba[ext=m4a]/ba",
			"--max-filesize", "48M",
			"--no-playlist",
			"-o", outTemplate,
			"--print", "after_move:filepath",
			videoURL,
		)
	} else {
		// Select 720p HD MP4 (capped at 15 minutes)
		args = append(args,
			"--js-runtimes", "node",
			"--no-warnings",
			"--concurrent-fragments", "16",
			"--match-filter", "duration <= 900",
			"-f", "22/best[height<=720][ext=mp4]/best[ext=mp4]/best",
			"--max-filesize", "48M",
			"--no-playlist",
			"-o", outTemplate,
			"--print", "after_move:filepath",
			videoURL,
		)
	}

	cmd := exec.CommandContext(ctx, d.cmdName, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		call.err = fmt.Errorf("yt-dlp failed: %v | logs: %s", err, string(out))
		return "", call.err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	resolvedPath := strings.TrimSpace(lines[len(lines)-1])

	if resolvedPath == "" {
		if matches, gErr := filepath.Glob(globPattern); gErr == nil && len(matches) > 0 {
			resolvedPath = matches[0]
		}
	}

	if resolvedPath == "" {
		call.err = fmt.Errorf("output file missing (empty path)")
		return "", call.err
	}
	if _, err := os.Stat(resolvedPath); os.IsNotExist(err) {
		call.err = fmt.Errorf("output file missing at expected path: %s", resolvedPath)
		return "", call.err
	}

	call.path = resolvedPath
	return resolvedPath, nil
}
