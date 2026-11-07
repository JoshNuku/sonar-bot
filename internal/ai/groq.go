package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

type GroqClient struct {
	client *openai.Client
	model  string
}

type ExtractedQuery struct {
	SearchTerm  string   `json:"search_term"`
	IsSongQuery bool     `json:"is_song_query"`
	Reply       string   `json:"reply"`
	Suggestions []string `json:"suggestions"`
}

var (
	conversationalPrefixRe = regexp.MustCompile(`(?i)^(please\s+|pls\s+|can\s+you\s+|could\s+you\s+)?(send(\s+me)?|download|find(\s+me)?|get(\s+me)?|play|search(\s+for)?)\s+`)
	pureGreetings          = map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true,
		"sup": true, "good morning": true, "good afternoon": true,
		"good evening": true, "thanks": true, "thank you": true,
		"ok": true, "okay": true, "how are you": true, "whats up": true, "what's up": true,
	}
)

func NewGroqClient(apiKey, model string) *GroqClient {
	if model == "" {
		model = "openai/gpt-oss-20b"
	}
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = "https://api.groq.com/openai/v1"
	return &GroqClient{
		client: openai.NewClientWithConfig(cfg),
		model:  model,
	}
}

func fallbackDecision(raw string) *ExtractedQuery {
	cleaned := strings.TrimSpace(raw)
	lower := strings.ToLower(strings.Trim(cleaned, "?!., "))
	if lower == "" || pureGreetings[lower] {
		return &ExtractedQuery{
			IsSongQuery: false,
			Reply:       "Hey! 👋 I'm **Sonar**, your AI music & video assistant. Tell me any song or artist you want to download (e.g. `First Person Shooter`), or ask me for personalized recommendations!",
		}
	}

	cleaned = conversationalPrefixRe.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(strings.Trim(cleaned, `"'?!.`))
	return &ExtractedQuery{
		IsSongQuery: true,
		SearchTerm:  cleaned,
	}
}

func extractJSONBlock(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}
	return s
}

func (g *GroqClient) ParseQuery(ctx context.Context, userInput string, recentSongs []string) (*ExtractedQuery, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	historyContext := "User has no prior song history yet."
	if len(recentSongs) > 0 {
		historyContext = fmt.Sprintf("User's recent requested/downloaded songs (most recent first): %s", strings.Join(recentSongs, " | "))
	}

	systemPrompt := fmt.Sprintf(`You are Sonar, a friendly, music-savvy personal AI assistant on Telegram that helps users discover and download songs and videos.
%s

Analyze the user's message:
1. If the user is naming or searching for a specific song, artist, lyrics, or video (including standalone titles like "first person shooter", "time flies", "pono"), set "is_song_query": true, "search_term" to the clean "Artist - Song Title" (use their taste profile to disambiguate if helpful), "reply": "", and "suggestions": [].
2. If the user is asking for recommendations, trending songs, "my vibe", "what have I listened to", or chatting, set "is_song_query": false, write a personalized, engaging response in "reply" that references their listening taste when relevant (using ONLY real, well-known songs), and populate "suggestions" with 5 real "Artist - Song Title" strings tailored to their taste so they can tap to download them.
Respond with valid JSON ONLY on a single line:
{"search_term": "Artist - Song Title", "is_song_query": true, "reply": "", "suggestions": []}`, historyContext)

	resp, err := g.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: g.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userInput},
		},
		Temperature: 0.3,
	})
	if err != nil || len(resp.Choices) == 0 {
		return fallbackDecision(userInput), nil
	}

	rawContent := extractJSONBlock(strings.TrimSpace(resp.Choices[0].Message.Content))
	var result ExtractedQuery
	if err := json.Unmarshal([]byte(rawContent), &result); err != nil {
		return fallbackDecision(userInput), nil
	}

	result.SearchTerm = strings.TrimSpace(result.SearchTerm)
	result.Reply = strings.TrimSpace(result.Reply)

	if !result.IsSongQuery {
		if result.Reply == "" {
			return fallbackDecision(userInput), nil
		}
		return &result, nil
	}

	if result.SearchTerm == "" {
		return fallbackDecision(userInput), nil
	}

	return &result, nil
}
