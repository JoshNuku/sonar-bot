package twitter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"sonar/internal/database"
	"sonar/internal/music"
)

type Config struct {
	APIKey       string
	APISecret    string
	AccessToken  string
	AccessSecret string
	BotUsername  string // Telegram bot username for promo links (e.g. "SonarMusicBot")
}

type Client struct {
	cfg        Config
	httpClient *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// IsConfigured returns true if all required Twitter OAuth credentials are present.
func (c *Client) IsConfigured() bool {
	return c.cfg.APIKey != "" &&
		c.cfg.APISecret != "" &&
		c.cfg.AccessToken != "" &&
		c.cfg.AccessSecret != ""
}

func oauthPercentEncode(s string) string {
	encoded := url.QueryEscape(s)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

func generateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Client) generateAuthHeader(method, endpoint string) string {
	params := map[string]string{
		"oauth_consumer_key":     c.cfg.APIKey,
		"oauth_nonce":            generateNonce(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(time.Now().Unix(), 10),
		"oauth_token":            c.cfg.AccessToken,
		"oauth_version":          "1.0",
	}

	// 1. Sort parameters alphabetically
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var paramParts []string
	for _, k := range keys {
		paramParts = append(paramParts, fmt.Sprintf("%s=%s", oauthPercentEncode(k), oauthPercentEncode(params[k])))
	}
	paramString := strings.Join(paramParts, "&")

	// 2. Build Signature Base String
	baseString := fmt.Sprintf(
		"%s&%s&%s",
		strings.ToUpper(method),
		oauthPercentEncode(endpoint),
		oauthPercentEncode(paramString),
	)

	// 3. Compute HMAC-SHA1 signature
	signingKey := fmt.Sprintf("%s&%s", oauthPercentEncode(c.cfg.APISecret), oauthPercentEncode(c.cfg.AccessSecret))
	mac := hmac.New(sha1.New, []byte(signingKey))
	mac.Write([]byte(baseString))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	params["oauth_signature"] = signature

	// 4. Construct Authorization Header
	var headerParts []string
	for _, k := range keys {
		headerParts = append(headerParts, fmt.Sprintf(`%s="%s"`, oauthPercentEncode(k), oauthPercentEncode(params[k])))
	}
	headerParts = append(headerParts, fmt.Sprintf(`oauth_signature="%s"`, oauthPercentEncode(signature)))

	return "OAuth " + strings.Join(headerParts, ", ")
}

// PostTweet publishes a post to Twitter/X using the v2 endpoint.
func (c *Client) PostTweet(ctx context.Context, text string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("twitter credentials not configured")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("tweet text cannot be empty")
	}
	if len([]rune(text)) > 280 {
		return fmt.Errorf("tweet exceeds 280 character limit (%d chars)", len([]rune(text)))
	}

	endpoint := "https://api.twitter.com/2/tweets"
	payload := map[string]string{"text": text}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}

	authHeader := c.generateAuthHeader(http.MethodPost, endpoint)
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("twitter request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("twitter API error (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	log.Printf("[Twitter] Tweet posted successfully: %q", text)
	return nil
}

// PostTrendingDrop formats and tweets the current top trending tracks.
func (c *Client) PostTrendingDrop(ctx context.Context, tracks []music.Track) error {
	if len(tracks) == 0 {
		return fmt.Errorf("no tracks to tweet")
	}

	limit := 5
	if len(tracks) < limit {
		limit = len(tracks)
	}

	botLink := ""
	if c.cfg.BotUsername != "" {
		botLink = fmt.Sprintf("https://t.me/%s", strings.TrimPrefix(c.cfg.BotUsername, "@"))
	}

	var sb strings.Builder
	sb.WriteString("Top Trending Tracks Today\n\n")

	for i := 0; i < limit; i++ {
		t := tracks[i]
		sb.WriteString(fmt.Sprintf("%d. %s - %s\n", i+1, t.Artist, t.Title))
	}

	if botLink != "" {
		sb.WriteString("\nDownload free on Telegram:\n" + botLink)
	}

	return c.PostTweet(ctx, sb.String())
}

// PostTopDownloaded formats and tweets the most popular downloads on Sonar.
func (c *Client) PostTopDownloaded(ctx context.Context, songs []database.TopSong) error {
	if len(songs) == 0 {
		return fmt.Errorf("no top songs to tweet")
	}

	limit := 5
	if len(songs) < limit {
		limit = len(songs)
	}

	botLink := ""
	if c.cfg.BotUsername != "" {
		botLink = fmt.Sprintf("https://t.me/%s", strings.TrimPrefix(c.cfg.BotUsername, "@"))
	}

	var sb strings.Builder
	sb.WriteString("Most Downloaded on Sonar This Week\n\n")

	for i := 0; i < limit; i++ {
		s := songs[i]
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s.Title))
	}

	if botLink != "" {
		sb.WriteString("\nGet yours instantly on Telegram:\n" + botLink)
	}

	return c.PostTweet(ctx, sb.String())
}
