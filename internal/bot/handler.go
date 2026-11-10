package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"sonar/internal/ai"
	"sonar/internal/database"
	"sonar/internal/media"
	"sonar/internal/music"
	"sonar/internal/payment"
)

type BotController struct {
	bot      *tgbotapi.BotAPI
	db       *database.DB
	groq     *ai.GroqClient
	dl       *media.Downloader
	paystack *payment.PaystackService
	deezer   *music.DeezerClient
	notifier *Notifier
	chatMu   sync.Mutex
	chatMem  map[int64][]ai.ChatTurn
}

func NewBotController(
	bot *tgbotapi.BotAPI,
	db *database.DB,
	groq *ai.GroqClient,
	dl *media.Downloader,
	ps *payment.PaystackService,
	deezer *music.DeezerClient,
	ntfyURL string,
) *BotController {
	return &BotController{
		bot:      bot,
		db:       db,
		groq:     groq,
		dl:       dl,
		paystack: ps,
		deezer:   deezer,
		notifier: NewNotifier(ntfyURL),
		chatMem:  make(map[int64][]ai.ChatTurn),
	}
}

func (b *BotController) getChatHistory(chatID int64) []ai.ChatTurn {
	b.chatMu.Lock()
	defer b.chatMu.Unlock()
	turns := b.chatMem[chatID]
	out := make([]ai.ChatTurn, len(turns))
	copy(out, turns)
	return out
}

func (b *BotController) recordChatTurn(chatID int64, userMsg, assistantMsg string) {
	b.chatMu.Lock()
	defer b.chatMu.Unlock()
	turns := append(b.chatMem[chatID], ai.ChatTurn{
		UserMsg:      userMsg,
		AssistantMsg: assistantMsg,
	})
	if len(turns) > 6 {
		turns = turns[len(turns)-6:]
	}
	b.chatMem[chatID] = turns
}

var youtubeURLRegex = regexp.MustCompile(`(?i)(?:https?:\/\/)?(?:www\.|m\.)?(?:youtube\.com\/(?:watch\?(?:.*&)?v=|embed\/|v\/|shorts\/)|youtu\.be\/)([\w-]{11})`)

func extractYouTubeVideoID(text string) string {
	matches := youtubeURLRegex.FindStringSubmatch(text)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func youtubeThumbnailURL(videoID string) string {
	return fmt.Sprintf("https://i.ytimg.com/vi/%s/hqdefault.jpg", videoID)
}

func downloadThumbnailBytes(ctx context.Context, videoID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, youtubeThumbnailURL(videoID), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thumbnail status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	for {
		orig := s
		s = strings.Trim(s, "*_`~ \t\r\n")
		s = strings.TrimPrefix(s, "🎧")
		s = strings.TrimPrefix(s, "🎵")
		s = strings.TrimPrefix(s, "🎬")
		s = strings.TrimPrefix(s, "⚡")
		s = strings.TrimPrefix(s, "Found:")
		s = strings.TrimSpace(s)
		if idx := strings.Index(strings.ToLower(s), "confirm your song for:"); idx != -1 {
			s = strings.TrimSpace(s[idx+len("confirm your song for:"):])
		}
		if s == orig {
			break
		}
	}
	return strings.TrimSpace(s)
}

func parseButtonTrackTitle(text string) string {
	clean := cleanTitle(text)
	// Strip leading numbering like "1. ", "2. ", "3. "
	if idx := strings.Index(clean, ". "); idx != -1 && idx <= 3 {
		clean = strings.TrimSpace(clean[idx+2:])
	}
	// Strip trailing duration like " (3:15)"
	if idx := strings.LastIndex(clean, "("); idx != -1 && strings.HasSuffix(clean, ")") {
		clean = strings.TrimSpace(clean[:idx])
	}
	if idx := strings.LastIndex(clean, "["); idx != -1 && strings.HasSuffix(clean, "]") {
		clean = strings.TrimSpace(clean[:idx])
	}
	return clean
}

func extractTrackTitle(msg *tgbotapi.Message, videoID string, dl *media.Downloader) string {
	// 1. Check in-memory track cache
	if info, ok := dl.GetCachedTrackInfo(videoID); ok && info.Title != "" {
		cleaned := cleanTitle(info.Title)
		if !strings.Contains(cleaned, "Confirm your song") {
			return cleaned
		}
	}

	// 2. Check inline keyboard buttons (the button the user tapped has the exact song title!)
	if msg != nil && msg.ReplyMarkup != nil {
		for _, row := range msg.ReplyMarkup.InlineKeyboard {
			for _, btn := range row {
				if btn.CallbackData != nil {
					cb := *btn.CallbackData
					if cb == fmt.Sprintf("select:%s", videoID) || cb == fmt.Sprintf("audio:%s", videoID) || cb == fmt.Sprintf("video:%s", videoID) {
						parsed := parseButtonTrackTitle(btn.Text)
						if parsed != "" && !strings.Contains(parsed, "Confirm your song") && !strings.Contains(parsed, "Audio (M4A)") && !strings.Contains(parsed, "Video (720p)") {
							dl.RememberTrack(media.TrackInfo{ID: videoID, Title: parsed})
							return parsed
						}
					}
				}
			}
		}
	}

	// 3. Check message text / caption for "Found:"
	if msg != nil {
		raw := msg.Caption
		if raw == "" {
			raw = msg.Text
		}
		lines := strings.Split(strings.TrimSpace(raw), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Found:") && i+1 < len(lines) {
				candidate := cleanTitle(lines[i+1])
				if candidate != "" && !strings.Contains(candidate, "Confirm your song") {
					dl.RememberTrack(media.TrackInfo{ID: videoID, Title: candidate})
					return candidate
				}
			}
		}
	}

	// 4. Guaranteed fallback: Fetch real title from YouTube oEmbed API
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if title := dl.ResolveTitle(ctx, videoID); title != "" {
		cleaned := cleanTitle(title)
		if cleaned != "" {
			return cleaned
		}
	}

	return "Sonar Track"
}

func sanitizeFileName(title, ext string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", " -", "*", "",
		"?", "", "\"", "'", "<", "", ">", "", "|", "-",
	)
	clean := strings.TrimSpace(replacer.Replace(title))
	if clean == "" {
		clean = "Sonar Track"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return clean + ext
}

func truncateButtonLabel(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

func (b *BotController) activateVerifiedTransaction(ctx context.Context, chatID int64, tx *payment.TransactionData) bool {
	if tx == nil || tx.Reference == "" {
		return false
	}
	plan := b.paystack.ResolvePlan(tx.Metadata.PlanTier, tx.Amount)
	paidAmount := tx.Amount
	if paidAmount <= 0 {
		paidAmount = plan.Amount
	}

	targetChatID := chatID
	if tx.Metadata.TelegramChatID != 0 {
		targetChatID = int64(tx.Metadata.TelegramChatID)
	}

	expiresAt, err := b.db.ProcessPaymentSuccess(ctx, tx.Reference, targetChatID, paidAmount, string(plan.Tier), plan.Duration)
	if err != nil || expiresAt.IsZero() {
		return false
	}

	msg := fmt.Sprintf(
		"🎉 **%s Activated!**\n\nUnlimited Sonar downloads unlocked until **%s UTC**. Send any song title to start downloading!",
		plan.Label,
		expiresAt.UTC().Format("02 Jan 2006 15:04"),
	)
	reply := tgbotapi.NewMessage(targetChatID, msg)
	reply.ParseMode = "Markdown"
	b.bot.Send(reply)
	b.notifier.NotifyPayment(targetChatID, plan.Label, b.paystack.Currency(), payment.FormatSubunits(paidAmount))
	return true
}

func (b *BotController) pollPaymentStatus(chatID int64, reference string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	timeout := time.After(5 * time.Minute)

	for {
		select {
		case <-timeout:
			return
		case <-ticker.C:
			tx, err := b.paystack.VerifyTransaction(reference)
			if err == nil && tx != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				b.activateVerifiedTransaction(ctx, chatID, tx)
				cancel()
				return
			}
		}
	}
}

func (b *BotController) NotifyStartup() {
	if b.notifier != nil {
		b.notifier.NotifyStartup()
	}
}

// StartPaymentWatcher reconciles successful Paystack transactions automatically in the background
// and immediately sends the Telegram subscription confirmation message without any user action.
func (b *BotController) StartPaymentWatcher() {
	reconcile := func() {
		txs, err := b.paystack.RecentSonarTransactions()
		if err != nil {
			return
		}
		for i := range txs {
			tx := txs[i]
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			b.activateVerifiedTransaction(ctx, int64(tx.Metadata.TelegramChatID), &tx)
			cancel()
		}
	}

	// Run immediately on startup to catch any payment completed while offline/restarting
	reconcile()

	ticker := time.NewTicker(6 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		reconcile()
	}
}

func (b *BotController) sendPaywall(chatID int64) {
	currency := b.paystack.Currency()
	dayPlan, _ := b.paystack.GetPlan(payment.TierDay)
	weekPlan, _ := b.paystack.GetPlan(payment.TierWeekly)
	monthPlan, _ := b.paystack.GetPlan(payment.TierMonthly)

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("⚡ Day Pass (24h) — %s %s", currency, payment.FormatSubunits(dayPlan.Amount)),
				"pay:day",
			),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("🔥 Weekly Pass (7d) — %s %s", currency, payment.FormatSubunits(weekPlan.Amount)),
				"pay:weekly",
			),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("👑 Monthly Unlimited (30d) — %s %s", currency, payment.FormatSubunits(monthPlan.Amount)),
				"pay:monthly",
			),
		),
	)

	msgText := fmt.Sprintf(
		"🔒 **Daily Free Quota Reached (%d/%d)**\n\nChoose a Sonar Pass to unlock unlimited downloads via Mobile Money:",
		b.db.DailyFreeLimit, b.db.DailyFreeLimit,
	)
	reply := tgbotapi.NewMessage(chatID, msgText)
	reply.ParseMode = "Markdown"
	reply.ReplyMarkup = kb
	b.bot.Send(reply)
}

func (b *BotController) HandleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	username := msg.From.UserName
	text := strings.TrimSpace(msg.Text)

	b.notifier.NotifyUserActivity(chatID, username, text)

	if strings.HasPrefix(text, "/start") || strings.HasPrefix(text, "/help") {
		welcome := "🎧 **Welcome to Sonar!**\n\nDownload any **YouTube Video** or **Song** directly inside Telegram.\n\n• **Direct Link:** Paste any YouTube or Shorts link to download instantly\n• **Search:** Type any song title, creator, or video name (e.g. `First Person Shooter`, `MKBHD`, `Lo-fi beats`)\n• **Formats:** Clean **M4A Audio** or **720p Video**\n\n💡 *Tip: Type /trending to see top music charts, or /history to view recent downloads.*\n\n`/terms`"
		reply := tgbotapi.NewMessage(chatID, welcome)
		reply.ParseMode = "Markdown"
		b.bot.Send(reply)
		return
	}

	if strings.HasPrefix(text, "/terms") {
		legalText := "⚖️ **Sonar Terms of Use & DMCA Notice**\n\n" +
			"1. **Nature of Service**: Sonar is an automated, user-directed AI discovery and ephemeral stream-indexing utility. Paid passes cover AI compute credits and cloud processing bandwidth only.\n" +
			"2. **Zero Media Hosting**: Sonar does not host, store, or maintain any audio or video files on its servers. All temporary streams are purged immediately upon delivery.\n" +
			"3. **Personal & Fair Use**: Users are solely responsible for ensuring their requests comply with applicable copyright laws and are for personal, non-commercial time-shifting only (tracks are capped at 15 minutes).\n" +
			"4. **DMCA / Takedown Requests**: Rights holders may request removal of any indexed reference ID by contacting the bot administrator with the relevant video ID."
		reply := tgbotapi.NewMessage(chatID, legalText)
		reply.ParseMode = "Markdown"
		b.bot.Send(reply)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Direct YouTube Link Detection (e.g. youtube.com/watch?v=..., youtu.be/..., youtube.com/shorts/...)
	if videoID := extractYouTubeVideoID(text); videoID != "" {
		allowed, _, _, quotaErr := b.db.CheckQuota(ctx, chatID, username)
		if quotaErr != nil {
			log.Printf("Quota error on youtube link: %v", quotaErr)
			return
		}
		if !allowed {
			b.sendPaywall(chatID)
			return
		}
		b.presentDirectVideoOptions(ctx, chatID, username, videoID)
		return
	}

	// Direct Deezer Live Chart Lookup for trending queries
	lowerText := strings.ToLower(text)
	if strings.HasPrefix(lowerText, "/trending") || lowerText == "trending" || lowerText == "what's trending" || lowerText == "trending songs" || lowerText == "top songs" || lowerText == "charts" {
		tracks, err := b.deezer.GetTopTrending(ctx, 5)
		if err == nil && len(tracks) > 0 {
			var sb strings.Builder
			sb.WriteString("🔥 **Top Trending Songs (Global Deezer Chart):**\n\n")
			var rows [][]tgbotapi.InlineKeyboardButton
			for i, t := range tracks {
				sb.WriteString(fmt.Sprintf("%d. **%s** — %s\n", i+1, t.Artist, t.Title))
				songLabel := fmt.Sprintf("%s - %s", t.Artist, t.Title)
				cbData := fmt.Sprintf("search:%s", truncateButtonLabel(songLabel, 54))
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(truncateButtonLabel("🎧 "+songLabel, 58), cbData),
				))
			}
			sb.WriteString("\nTap any track below to fetch audio or video:")
			reply := tgbotapi.NewMessage(chatID, sb.String())
			reply.ParseMode = "Markdown"
			reply.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
			b.bot.Send(reply)
			return
		}
	}

	// Show native Telegram typing status while evaluating
	b.bot.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

	allowed, _, recentSongs, quotaErr := b.db.CheckQuota(ctx, chatID, username)

	if strings.HasPrefix(text, "/history") {
		if len(recentSongs) == 0 {
			b.bot.Send(tgbotapi.NewMessage(chatID, "🎶 You haven't downloaded any tracks yet. Send a song title like `First Person Shooter` to get started!"))
			return
		}
		var rows [][]tgbotapi.InlineKeyboardButton
		limit := len(recentSongs)
		if limit > 5 {
			limit = 5
		}
		for i := 0; i < limit; i++ {
			s := recentSongs[i]
			cbData := fmt.Sprintf("search:%s", truncateButtonLabel(s, 54))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(truncateButtonLabel("🔁 "+s, 58), cbData),
			))
		}
		kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
		msg := tgbotapi.NewMessage(chatID, "🎧 **Your Recent Sonar History:**\nTap any track to grab it again:")
		msg.ParseMode = "Markdown"
		msg.ReplyMarkup = kb
		b.bot.Send(msg)
		return
	}

	parsed, parseErr := b.groq.ParseQuery(ctx, text, recentSongs, b.getChatHistory(chatID))

	if !allowed {
		b.sendPaywall(chatID)
		return
	}

	if parseErr != nil || parsed == nil {
		log.Printf("Groq parse error for %q: %v", text, parseErr)
		b.bot.Send(tgbotapi.NewMessage(chatID, "Couldn't process your message right now. Try sending a song title like 'First Person Shooter'."))
		return
	}

	// Conversational AI reply if the user is chatting or asking for recommendations (does not consume download quota)
	if !parsed.IsSongQuery {
		b.recordChatTurn(chatID, text, parsed.Reply)
		reply := tgbotapi.NewMessage(chatID, parsed.Reply)
		reply.ParseMode = "Markdown"

		if len(parsed.Suggestions) > 0 {
			// Validate suggestions with Deezer to guarantee real, released tracks
			if validated := b.deezer.ValidateSuggestions(ctx, parsed.Suggestions); len(validated) > 0 {
				parsed.Suggestions = validated
			}
		}

		if len(parsed.Suggestions) > 3 {
			parsed.Suggestions = parsed.Suggestions[:3]
		}

		if len(parsed.Suggestions) > 0 {
			var rows [][]tgbotapi.InlineKeyboardButton
			for _, s := range parsed.Suggestions {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				cbData := fmt.Sprintf("search:%s", truncateButtonLabel(s, 54))
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(truncateButtonLabel("🎧 "+s, 58), cbData),
				))
			}
			if len(rows) > 0 {
				kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
				reply.ReplyMarkup = kb
			}
		}

		if _, err := b.bot.Send(reply); err != nil {
			reply.ParseMode = ""
			b.bot.Send(reply)
		}
		return
	}

	if quotaErr != nil {
		log.Printf("Quota evaluation error: %v", quotaErr)
		return
	}

	query := parsed.SearchTerm
	b.recordChatTurn(chatID, text, "Searched YouTube for: "+query)
	tracks, err := b.dl.ResolveTracks(ctx, query, 5)
	if err != nil || len(tracks) == 0 {
		log.Printf("ResolveTracks error for %q: %v", query, err)
		b.bot.Send(tgbotapi.NewMessage(chatID, "No matching results found on YouTube."))
		return
	}

	// Present Top 5 matching songs so the user can confirm the exact track they want
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, t := range tracks {
		dur := ""
		if t.Duration != "" {
			dur = fmt.Sprintf(" (%s)", t.Duration)
		}
		btnText := truncateButtonLabel(fmt.Sprintf("%d. %s%s", i+1, t.Title, dur), 58)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(btnText, fmt.Sprintf("select:%s", t.ID)),
		))
	}

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	prompt := fmt.Sprintf("🎧 **Confirm your song for:** `%s`\n\nSelect a track below:", query)
	confirmMsg := tgbotapi.NewMessage(chatID, prompt)
	confirmMsg.ParseMode = "Markdown"
	confirmMsg.ReplyMarkup = kb
	b.bot.Send(confirmMsg)
}

func formatPercent(percent int) string {
	if percent > 100 {
		percent = 100
	}
	if percent < 0 {
		percent = 0
	}
	return fmt.Sprintf("%d%%", percent)
}

type progressReader struct {
	r      io.Reader
	total  int64
	read   int64
	onProg func(pct int)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.read += int64(n)
	if pr.total > 0 && pr.onProg != nil {
		pct := int((pr.read * 100) / pr.total)
		pr.onProg(pct)
	}
	return n, err
}

func (b *BotController) presentDirectVideoOptions(ctx context.Context, chatID int64, username, videoID string) {
	b.dl.PrefetchAudio(videoID)
	trackTitle := b.dl.ResolveTitle(ctx, videoID)
	if trackTitle == "" {
		trackTitle = "YouTube Video"
	} else {
		trackTitle = cleanTitle(trackTitle)
	}
	b.dl.RememberTrack(media.TrackInfo{ID: videoID, Title: trackTitle})
	b.db.RecordUserSong(ctx, chatID, trackTitle)

	_, remaining, _, _ := b.db.CheckQuota(ctx, chatID, username)

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎵 Audio (M4A)", fmt.Sprintf("audio:%s", videoID)),
			tgbotapi.NewInlineKeyboardButtonData("🎬 Video (720p)", fmt.Sprintf("video:%s", videoID)),
		),
	)

	quotaLine := fmt.Sprintf("Downloads left today: %d", remaining)
	if remaining >= 999 {
		quotaLine = "Pass Status: ⚡ Unlimited Active"
	}
	prompt := fmt.Sprintf("Found:\n**%s**\n\n%s\nSelect format:", trackTitle, quotaLine)

	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(youtubeThumbnailURL(videoID)))
	photo.Caption = prompt
	photo.ParseMode = "Markdown"
	photo.ReplyMarkup = kb
	if _, err := b.bot.Send(photo); err != nil {
		msg := tgbotapi.NewMessage(chatID, prompt)
		msg.ParseMode = "Markdown"
		msg.ReplyMarkup = kb
		b.bot.Send(msg)
	}
}

func (b *BotController) HandleCallback(cb *tgbotapi.CallbackQuery) {
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		return
	}
	action, value := parts[0], parts[1]
	chatID := cb.Message.Chat.ID

	// 1. Handle AI Song Suggestion Click ("search:<query>")
	if action == "search" {
		b.bot.Request(tgbotapi.NewCallback(cb.ID, "Searching YouTube..."))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		allowed, _, _, err := b.db.CheckQuota(ctx, chatID, cb.From.UserName)
		if err != nil {
			return
		}
		if !allowed {
			b.sendPaywall(chatID)
			return
		}

		tracks, err := b.dl.ResolveTracks(ctx, value, 5)
		if err != nil || len(tracks) == 0 {
			b.bot.Send(tgbotapi.NewMessage(chatID, "No matching results found on YouTube."))
			return
		}

		var rows [][]tgbotapi.InlineKeyboardButton
		for i, t := range tracks {
			dur := ""
			if t.Duration != "" {
				dur = fmt.Sprintf(" (%s)", t.Duration)
			}
			btnText := truncateButtonLabel(fmt.Sprintf("%d. %s%s", i+1, t.Title, dur), 58)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(btnText, fmt.Sprintf("select:%s", t.ID)),
			))
		}

		kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
		prompt := fmt.Sprintf("🎧 **Confirm your song for:** `%s`\n\nSelect a track below:", value)
		reply := tgbotapi.NewMessage(chatID, prompt)
		reply.ParseMode = "Markdown"
		reply.ReplyMarkup = kb
		b.bot.Send(reply)
		return
	}

	// 2. Handle Song Confirmation Selection ("select:<videoID>")
	if action == "select" {
		b.bot.Request(tgbotapi.NewCallback(cb.ID, "Track selected!"))
		videoID := value

		// Immediately start prefetching the audio stream in the background while the user chooses format!
		b.dl.PrefetchAudio(videoID)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		_, remaining, _, _ := b.db.CheckQuota(ctx, chatID, cb.From.UserName)
		trackTitle := extractTrackTitle(cb.Message, videoID, b.dl)
		b.db.RecordUserSong(ctx, chatID, trackTitle)

		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🎵 Audio (M4A)", fmt.Sprintf("audio:%s", videoID)),
				tgbotapi.NewInlineKeyboardButtonData("🎬 Video (720p)", fmt.Sprintf("video:%s", videoID)),
			),
		)

		quotaLine := fmt.Sprintf("Downloads left today: %d", remaining)
		if remaining >= 999 {
			quotaLine = "Pass Status: ⚡ Unlimited Active"
		}
		prompt := fmt.Sprintf("Found:\n**%s**\n\n%s\nSelect format:", trackTitle, quotaLine)

		photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(youtubeThumbnailURL(videoID)))
		photo.Caption = prompt
		photo.ParseMode = "Markdown"
		photo.ReplyMarkup = kb
		if _, photoErr := b.bot.Send(photo); photoErr == nil {
			b.bot.Send(tgbotapi.NewDeleteMessage(chatID, cb.Message.MessageID))
			return
		}

		edit := tgbotapi.NewEditMessageText(chatID, cb.Message.MessageID, prompt)
		edit.ParseMode = "Markdown"
		edit.ReplyMarkup = &kb
		b.bot.Send(edit)
		return
	}

	// 3. Handle Tier Payment Checkout Callbacks ("pay:<tier>")
	if action == "pay" {
		b.bot.Request(tgbotapi.NewCallback(cb.ID, "Generating Mobile Money link..."))
		tier := payment.PlanTier(value)
		plan, ok := b.paystack.GetPlan(tier)
		if !ok {
			return
		}

		payURL, reference, err := b.paystack.CreateMoMoLink(chatID, cb.From.UserName, tier)
		if err != nil {
			b.bot.Send(tgbotapi.NewMessage(chatID, "⚠️ Unable to generate MoMo payment link. Try again soon."))
			return
		}

		// Automatically poll Paystack in the background so localhost testing & webhooks push the confirmation message automatically!
		go b.pollPaymentStatus(chatID, reference)

		btnLabel := fmt.Sprintf("💳 Pay %s %s (%s)", b.paystack.Currency(), payment.FormatSubunits(plan.Amount), plan.Label)
		payBtn := tgbotapi.NewInlineKeyboardButtonURL(btnLabel, payURL)
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(payBtn),
		)

		reply := tgbotapi.NewMessage(chatID, fmt.Sprintf("Selected **%s**.\nTap below to complete your Mobile Money checkout:", plan.Label))
		reply.ParseMode = "Markdown"
		reply.ReplyMarkup = kb
		b.bot.Send(reply)
		return
	}

	// 4. Handle Media Download ("audio:<videoID>" or "video:<videoID>")
	b.bot.Request(tgbotapi.NewCallback(cb.ID, "Fetching media..."))
	format, videoID := action, value
	cacheFormat := format
	if format == "audio" {
		cacheFormat = "audio_hq"
	}
	trackTitle := extractTrackTitle(cb.Message, videoID, b.dl)
	b.notifier.NotifyDownload(chatID, cb.From.UserName, trackTitle, format)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Fastpath: Check Neon Cache
	cachedFileID, title, err := b.db.GetCachedFileID(ctx, videoID, cacheFormat)
	if err == nil && cachedFileID != "" {
		b.db.RecordUserSong(ctx, chatID, title)
		if format == "audio" {
			audio := tgbotapi.NewAudio(chatID, tgbotapi.FileID(cachedFileID))
			audio.Caption = fmt.Sprintf("⚡ %s", title)
			b.bot.Send(audio)
		} else {
			video := tgbotapi.NewVideo(chatID, tgbotapi.FileID(cachedFileID))
			video.Caption = fmt.Sprintf("⚡ %s", title)
			b.bot.Send(video)
		}
		_ = b.db.IncrementUsage(ctx, chatID)
		return
	}

	// Cache Miss: Download stream & thumbnail in parallel
	currentPct := 12
	statusMsg, _ := b.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("⏳ Downloading %s... (%s)", trackTitle, formatPercent(currentPct))))

	var (
		filePath   string
		dlErr      error
		thumbBytes []byte
		thumbErr   error
		wg         sync.WaitGroup
		editMu     sync.Mutex
		lastEdit   = time.Now()
		dlDone     = make(chan struct{})
	)

	updateDownloadMsg := func(pct int) {
		editMu.Lock()
		defer editMu.Unlock()
		if pct <= currentPct && pct < 100 {
			return
		}
		currentPct = pct
		now := time.Now()
		if now.Sub(lastEdit) < 1200*time.Millisecond && pct < 100 {
			return
		}
		lastEdit = now
		text := fmt.Sprintf("⏳ Downloading %s... (%s)", trackTitle, formatPercent(currentPct))
		edit := tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, text)
		_, _ = b.bot.Send(edit)
	}

	// Smooth progressive ticker so user sees continuous progress instead of staying at 0%
	go func() {
		ticker := time.NewTicker(1200 * time.Millisecond)
		defer ticker.Stop()
		steps := []int{28, 46, 67, 83, 92, 96}
		stepIdx := 0
		for {
			select {
			case <-dlDone:
				return
			case <-ticker.C:
				if stepIdx < len(steps) {
					updateDownloadMsg(steps[stepIdx])
					stepIdx++
				}
			}
		}
	}()

	onDownloadProgress := func(pct int) {
		updateDownloadMsg(pct)
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		filePath, dlErr = b.dl.DownloadStream(ctx, videoID, format, onDownloadProgress)
	}()
	go func() {
		defer wg.Done()
		thumbBytes, thumbErr = downloadThumbnailBytes(ctx, videoID)
	}()
	wg.Wait()
	close(dlDone)

	if dlErr != nil {
		log.Printf("DownloadStream error (%s, %s): %v", videoID, format, dlErr)
		b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "❌ Failed: Stream unavailable or exceeds 48MB."))
		return
	}
	defer os.Remove(filePath)

	b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "📤 Uploading to Telegram... (0%)"))

	ext := filepath.Ext(filePath)
	if ext == "" {
		if format == "audio" {
			ext = ".m4a"
		} else {
			ext = ".mp4"
		}
	}
	uploadName := sanitizeFileName(trackTitle, ext)

	var thumb []byte
	if thumbErr == nil && len(thumbBytes) > 0 {
		thumb = thumbBytes
	}

	onUploadProgress := func(pct int) {
		editMu.Lock()
		defer editMu.Unlock()
		now := time.Now()
		if now.Sub(lastEdit) < 1300*time.Millisecond && pct < 100 {
			return
		}
		lastEdit = now
		text := fmt.Sprintf("📤 Uploading to Telegram... (%s)", formatPercent(pct))
		edit := tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, text)
		_, _ = b.bot.Send(edit)
	}

	fileID, uploadErr := b.sendMediaBuffered(ctx, chatID, format, filePath, uploadName, trackTitle, thumb, onUploadProgress)
	b.bot.Send(tgbotapi.NewDeleteMessage(chatID, statusMsg.MessageID))

	if uploadErr != nil {
		log.Printf("Telegram upload error (%s, %s): %v", videoID, format, uploadErr)
		return
	}

	go func(fID string) {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if fID != "" {
			if cacheErr := b.db.SaveCachedFileID(bgCtx, videoID, cacheFormat, fID, trackTitle); cacheErr != nil {
				log.Printf("SaveCachedFileID error (%s, %s): %v", videoID, cacheFormat, cacheErr)
			}
		}
		b.db.RecordUserSong(bgCtx, chatID, trackTitle)
		_ = b.db.IncrementUsage(bgCtx, chatID)
	}(fileID)
}

func (b *BotController) sendMediaBuffered(
	ctx context.Context,
	chatID int64,
	format string,
	filePath string,
	uploadName string,
	trackTitle string,
	thumbBytes []byte,
	onProgress func(pct int),
) (string, error) {
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	buf.Grow(len(fileData) + len(thumbBytes) + 4096)
	mw := multipart.NewWriter(&buf)

	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))

	endpoint := "sendAudio"
	fieldName := "audio"
	if format == "audio" {
		_ = mw.WriteField("caption", fmt.Sprintf("🎧 %s", trackTitle))
		if parts := strings.SplitN(trackTitle, " - ", 2); len(parts) == 2 {
			_ = mw.WriteField("performer", strings.TrimSpace(parts[0]))
			_ = mw.WriteField("title", strings.TrimSpace(parts[1]))
		} else {
			_ = mw.WriteField("title", trackTitle)
		}
	} else {
		endpoint = "sendVideo"
		fieldName = "video"
		_ = mw.WriteField("caption", fmt.Sprintf("🎬 %s", trackTitle))
		_ = mw.WriteField("supports_streaming", "true")
	}

	part, err := mw.CreateFormFile(fieldName, uploadName)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(fileData); err != nil {
		return "", err
	}

	if len(thumbBytes) > 0 {
		if thumbPart, tErr := mw.CreateFormFile("thumb", "thumb.jpg"); tErr == nil {
			_, _ = thumbPart.Write(thumbBytes)
		}
	}

	if err := mw.Close(); err != nil {
		return "", err
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.bot.Token, endpoint)
	pr := &progressReader{
		r:      &buf,
		total:  int64(buf.Len()),
		onProg: onProgress,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(buf.Len())

	uploadClient := &http.Client{Timeout: 3 * time.Minute}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var apiResp tgbotapi.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", err
	}
	if !apiResp.Ok {
		return "", fmt.Errorf("telegram api error: %s", apiResp.Description)
	}

	var msg tgbotapi.Message
	if err := json.Unmarshal(apiResp.Result, &msg); err == nil {
		if msg.Audio != nil {
			return msg.Audio.FileID, nil
		}
		if msg.Video != nil {
			return msg.Video.FileID, nil
		}
	}
	return "", nil
}
