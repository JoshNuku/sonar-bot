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

type ChatTurn struct {
	UserMsg      string
	AssistantMsg string
}

var (
	conversationalPrefixRe = regexp.MustCompile(`(?i)^(please\s+|pls\s+|can\s+you\s+|could\s+you\s+|i\s+want(\s+the)?\s+|give\s+me(\s+the)?\s+)?(send(\s+me)?|download|find(\s+me)?|get(\s+me)?|play|search(\s+for)?)\s+`)
	pureGreetings          = map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true,
		"sup": true, "good morning": true, "good afternoon": true,
		"good evening": true, "thanks": true, "thank you": true,
		"ok": true, "okay": true, "how are you": true, "whats up": true, "what's up": true,
	}
	versionModifiers = []struct {
		match []string
		label string
	}{
		{match: []string{"instrumental", "instrumentals"}, label: "Instrumental"},
		{match: []string{"sped up", "speed up", "nightcore"}, label: "Sped Up"},
		{match: []string{"slowed", "reverb"}, label: "Slowed & Reverb"},
		{match: []string{"acapella", "a cappella", "vocals only"}, label: "Acapella"},
		{match: []string{"karaoke"}, label: "Karaoke"},
		{match: []string{"acoustic"}, label: "Acoustic"},
		{match: []string{"remix"}, label: "Remix"},
		{match: []string{"live performance", "live version"}, label: "Live"},
	}
)

func NewGroqClient(apiKey, model string) *GroqClient {
	if model == "" {
		model = "qwen/qwen3.8-27b"
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

	conversational := map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true, "sup": true,
		"good morning": true, "good afternoon": true, "good evening": true,
		"thanks": true, "thank you": true, "ok": true, "okay": true, "k": true,
		"how are you": true, "whats up": true, "what's up": true,
		"no": true, "nope": true, "nah": true, "yes": true, "yeah": true, "yep": true,
		"what are you thinking": true, "what do you think": true, "nothing": true,
		"nevermind": true, "nvm": true, "idk": true, "who are you": true, "what are you": true,
		"lol": true, "lmao": true, "haha": true, "cool": true, "sure": true,
	}

	if lower == "" || conversational[lower] || strings.HasPrefix(lower, "what are you") || strings.HasPrefix(lower, "who are you") || strings.HasPrefix(lower, "how do you") {
		return &ExtractedQuery{
			IsSongQuery: false,
			Reply:       "I'm here! If you were looking for a song or video, just let me know the title or artist. Otherwise, what's on your mind?",
		}
	}

	cleaned = conversationalPrefixRe.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(strings.Trim(cleaned, `"'?!.`))
	if len(cleaned) <= 3 {
		return &ExtractedQuery{
			IsSongQuery: false,
			Reply:       "I'm listening! If that was a song title, tell me the artist and I'll find it for you.",
		}
	}

	return &ExtractedQuery{
		IsSongQuery: false,
		Reply:       fmt.Sprintf("Did you want me to search for a song called **%s**, or were you just chatting? If it's a song, just send the artist and title!", cleaned),
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

func preserveModifiers(userInput, searchTerm string) string {
	lowerInput := strings.ToLower(userInput)
	lowerSearch := strings.ToLower(searchTerm)
	for _, mod := range versionModifiers {
		matched := false
		for _, kw := range mod.match {
			if strings.Contains(lowerInput, kw) {
				matched = true
				break
			}
		}
		if matched && !strings.Contains(lowerSearch, strings.ToLower(mod.label)) {
			alreadyHas := false
			for _, kw := range mod.match {
				if strings.Contains(lowerSearch, kw) {
					alreadyHas = true
					break
				}
			}
			if !alreadyHas {
				searchTerm = strings.TrimSpace(searchTerm) + " " + mod.label
				lowerSearch = strings.ToLower(searchTerm)
			}
		}
	}
	return searchTerm
}

func (g *GroqClient) ParseQuery(ctx context.Context, userInput string, recentSongs []string, recentChat []ChatTurn) (*ExtractedQuery, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	historyContext := "User has no prior downloaded song history yet."
	if len(recentSongs) > 0 {
		historyContext = fmt.Sprintf("User's recent requested/downloaded songs (most recent first): %s", strings.Join(recentSongs, " | "))
	}

	var chatLines []string
	for _, turn := range recentChat {
		chatLines = append(chatLines, fmt.Sprintf("User: %s\nSonar: %s", turn.UserMsg, turn.AssistantMsg))
	}
	chatContext := "No previous messages in this session."
	if len(chatLines) > 0 {
		chatContext = "Recent conversation context (use this to understand follow-ups like 'by Drake', 'the instrumental version', 'for time flies', etc.):\n" + strings.Join(chatLines, "\n")
	}

	systemPrompt := fmt.Sprintf(`You are Sonar, an intelligent, music-savvy AI assistant on Telegram that helps users discover and download songs and videos.
%s

%s

Analyze the user's latest message carefully:
1. If searching for a specific song, artist, video, or version (e.g. 'drake time flies', 'o holy night', 'first person shooter', 'sped up', 'time flies instrumental'):
   - Combine with recent conversation context if they are continuing or refining a request (e.g. 'by Drake', 'the instrumental version', 'for time flies').
   - Keep requested version modifiers in "search_term" (e.g. "Instrumental", "Remix", "Slowed & Reverb", "Sped Up", "Acoustic", "Live", "Karaoke", "Acapella").
   - Set "is_song_query": true, "search_term": "Artist - Song Title", "reply": "", "suggestions": [].
2. If looking for a style, mood, genre, theme, or general recommendations (e.g. 'Christmas songs the old kind like o holy night', 'recommend me rap', 'what should I listen to', 'songs like X'):
   - Set "is_song_query": false.
   - Write an engaging, friendly "reply".
   - Populate "suggestions" with up to 3 real, matching "Artist - Song Title" strings so the user can tap to download them.
3. If casual chatting, greeting, reacting, or saying phrases like 'no', 'hi', 'come on you should have caught that', 'what are you thinking':
   - Set "is_song_query": false, write a witty, natural, conversational "reply", and set "suggestions": [].

Respond with valid JSON ONLY on a single line:
{"search_term": "Artist - Song Title", "is_song_query": true, "reply": "", "suggestions": []}`, historyContext, chatContext)

	resp, err := g.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: g.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userInput},
		},
		Temperature: 0.1,
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
			result.Reply = "I'm listening! Was that a song title you wanted me to find, or were you just chatting?"
		}
		return &result, nil
	}

	if result.SearchTerm == "" {
		return fallbackDecision(userInput), nil
	}

	result.SearchTerm = preserveModifiers(userInput, result.SearchTerm)
	return &result, nil
}
