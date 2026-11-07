package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"sonar/internal/ai"
	"sonar/internal/database"
	"sonar/internal/media"
	"sonar/internal/payment"
)

type BotController struct {
	bot      *tgbotapi.BotAPI
	db       *database.DB
	groq     *ai.GroqClient
	dl       *media.Downloader
	paystack *payment.PaystackService
	notifier *Notifier
}

func NewBotController(
	bot *tgbotapi.BotAPI,
	db *database.DB,
	groq *ai.GroqClient,
	dl *media.Downloader,
	ps *payment.PaystackService,
	ntfyURL string,
) *BotController {
	return &BotController{
		bot:      bot,
		db:       db,
		groq:     groq,
		dl:       dl,
		paystack: ps,
		notifier: NewNotifier(ntfyURL),
	}
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

func extractTrackTitle(msg *tgbotapi.Message, videoID string, dl *media.Downloader) string {
	if info, ok := dl.GetCachedTrackInfo(videoID); ok && info.Title != "" {
		return info.Title
	}
	if msg == nil {
		return "Sonar Track"
	}
	raw := msg.Caption
	if raw == "" {
		raw = msg.Text
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "Found:") {
		return strings.TrimSpace(lines[1])
	}
	if len(lines) > 0 && lines[0] != "" {
		return strings.TrimSpace(lines[0])
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

	if strings.HasPrefix(text, "/start") || strings.HasPrefix(text, "/help") {
		welcome := "🎧 **Welcome to Sonar!**\n\nSend me any song title, artist, or video name (e.g. `First Person Shooter`) and I'll fetch the high-quality **M4A Audio** or **720p Video** for you.\n\n💡 *Tip: Ask me for recommendations anytime—I learn your music taste from what you download!*\n\n`/terms`"
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

	b.notifier.NotifyUserActivity(chatID, username, text)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Send fast thinking indicator in background while checking quota & history
	statusCh := make(chan tgbotapi.Message, 1)
	go func() {
		m, _ := b.bot.Send(tgbotapi.NewMessage(chatID, "💭 Thinking..."))
		statusCh <- m
	}()

	allowed, _, recentSongs, quotaErr := b.db.CheckQuota(ctx, chatID, username)

	if strings.HasPrefix(text, "/history") {
		statusMsg := <-statusCh
		if len(recentSongs) == 0 {
			b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "🎶 You haven't downloaded any tracks yet. Send a song title like `First Person Shooter` to get started!"))
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
		edit := tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "🎧 **Your Recent Sonar History:**\nTap any track to grab it again:")
		edit.ParseMode = "Markdown"
		edit.ReplyMarkup = &kb
		b.bot.Send(edit)
		return
	}

	parsed, parseErr := b.groq.ParseQuery(ctx, text, recentSongs)
	statusMsg := <-statusCh

	if !allowed {
		if statusMsg.MessageID != 0 {
			b.bot.Send(tgbotapi.NewDeleteMessage(chatID, statusMsg.MessageID))
		}
		b.sendPaywall(chatID)
		return
	}

	if parseErr != nil || parsed == nil {
		log.Printf("Groq parse error for %q: %v", text, parseErr)
		b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "Couldn't process your message right now. Try sending a song title like 'First Person Shooter'."))
		return
	}

	// Conversational AI reply if the user is chatting or asking for recommendations (does not consume download quota)
	if !parsed.IsSongQuery {
		edit := tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, parsed.Reply)
		edit.ParseMode = "Markdown"

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
				edit.ReplyMarkup = &kb
			}
		}

		if _, err := b.bot.Send(edit); err != nil {
			edit.ParseMode = ""
			b.bot.Send(edit)
		}
		return
	}

	if quotaErr != nil {
		log.Printf("Quota evaluation error: %v", quotaErr)
		return
	}

	query := parsed.SearchTerm
	tracks, err := b.dl.ResolveTracks(ctx, query, 5)
	if err != nil || len(tracks) == 0 {
		log.Printf("ResolveTracks error for %q: %v", query, err)
		b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "No matching results found on YouTube."))
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
	edit := tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, prompt)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &kb
	b.bot.Send(edit)
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
	statusMsg, _ := b.bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("⏳ Downloading %s...", trackTitle)))

	var (
		filePath   string
		dlErr      error
		thumbBytes []byte
		thumbErr   error
		wg         sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		filePath, dlErr = b.dl.DownloadStream(ctx, videoID, format)
	}()
	go func() {
		defer wg.Done()
		thumbBytes, thumbErr = downloadThumbnailBytes(ctx, videoID)
	}()
	wg.Wait()

	if dlErr != nil {
		log.Printf("DownloadStream error (%s, %s): %v", videoID, format, dlErr)
		b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "❌ Failed: Stream unavailable or exceeds 48MB."))
		return
	}
	defer os.Remove(filePath)

	b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "📤 Uploading to Telegram..."))

	fileHandle, openErr := os.Open(filePath)
	if openErr != nil {
		b.bot.Send(tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, "❌ Failed to read downloaded media file."))
		return
	}
	defer fileHandle.Close()

	ext := filepath.Ext(filePath)
	if ext == "" {
		if format == "audio" {
			ext = ".m4a"
		} else {
			ext = ".mp4"
		}
	}
	namedUpload := tgbotapi.FileReader{
		Name:   sanitizeFileName(trackTitle, ext),
		Reader: fileHandle,
	}

	if format == "audio" {
		audioCfg := tgbotapi.NewAudio(chatID, namedUpload)
		if parts := strings.SplitN(trackTitle, " - ", 2); len(parts) == 2 {
			audioCfg.Performer = strings.TrimSpace(parts[0])
			audioCfg.Title = strings.TrimSpace(parts[1])
		} else {
			audioCfg.Title = trackTitle
		}
		audioCfg.Caption = fmt.Sprintf("🎧 %s", trackTitle)
		if thumbErr == nil && len(thumbBytes) > 0 {
			audioCfg.Thumb = tgbotapi.FileBytes{Name: "thumb.jpg", Bytes: thumbBytes}
		}
		msg, uploadErr := b.bot.Send(audioCfg)
		if uploadErr == nil && msg.Audio != nil {
			if cacheErr := b.db.SaveCachedFileID(ctx, videoID, cacheFormat, msg.Audio.FileID, trackTitle); cacheErr != nil {
				log.Printf("SaveCachedFileID error (%s, %s): %v", videoID, cacheFormat, cacheErr)
			}
		}
	} else {
		videoCfg := tgbotapi.NewVideo(chatID, namedUpload)
		videoCfg.Caption = fmt.Sprintf("🎬 %s", trackTitle)
		if thumbErr == nil && len(thumbBytes) > 0 {
			videoCfg.Thumb = tgbotapi.FileBytes{Name: "thumb.jpg", Bytes: thumbBytes}
		}
		msg, uploadErr := b.bot.Send(videoCfg)
		if uploadErr == nil && msg.Video != nil {
			if cacheErr := b.db.SaveCachedFileID(ctx, videoID, cacheFormat, msg.Video.FileID, trackTitle); cacheErr != nil {
				log.Printf("SaveCachedFileID error (%s, %s): %v", videoID, cacheFormat, cacheErr)
			}
		}
	}

	b.db.RecordUserSong(ctx, chatID, trackTitle)
	_ = b.db.IncrementUsage(ctx, chatID)
	b.bot.Send(tgbotapi.NewDeleteMessage(chatID, statusMsg.MessageID))
}
