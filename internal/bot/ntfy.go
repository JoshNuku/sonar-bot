package bot

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Notifier struct {
	ntfyURL      string
	tgBot        *tgbotapi.BotAPI
	adminChatID  int64
	chatCooldown time.Duration
	dlCooldown   time.Duration
	mu           sync.Mutex
	lastChat     map[int64]time.Time
	lastDownload map[int64]time.Time
	lastStatus   string
	lastSentAt   time.Time
	client       *http.Client
}

func NewNotifier(ntfyURL string, tgBot *tgbotapi.BotAPI, adminChatID int64) *Notifier {
	return &Notifier{
		ntfyURL:      strings.TrimRight(strings.TrimSpace(ntfyURL), "/"),
		tgBot:        tgBot,
		adminChatID:  adminChatID,
		chatCooldown: 30 * time.Minute, // Silent session alert once every 30 minutes per user
		dlCooldown:   1 * time.Minute,  // Debounce consecutive rapid downloads from same user
		lastChat:     make(map[int64]time.Time),
		lastDownload: make(map[int64]time.Time),
		client:       &http.Client{Timeout: 10 * time.Second},
	}
}

func formatUserTag(chatID int64, username string) string {
	if username != "" {
		return fmt.Sprintf("@%s (%d)", username, chatID)
	}
	return fmt.Sprintf("User %d", chatID)
}

func (n *Notifier) recordStatus(status string) {
	n.mu.Lock()
	n.lastStatus = status
	n.lastSentAt = time.Now().UTC()
	n.mu.Unlock()
}

// Status returns the configured notification target, last delivery status, and last attempt time.
func (n *Notifier) Status() (string, string, time.Time) {
	if n == nil {
		return "", "not configured", time.Time{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	target := n.ntfyURL
	if n.adminChatID != 0 {
		target = fmt.Sprintf("Telegram (%d)", n.adminChatID)
	}
	return target, n.lastStatus, n.lastSentAt
}

func (n *Notifier) sendTelegram(title, body, priority string) error {
	if n == nil || n.tgBot == nil || n.adminChatID == 0 {
		return fmt.Errorf("ADMIN_CHAT_ID not configured")
	}
	text := fmt.Sprintf("🔔 %s\n%s", title, body)
	msg := tgbotapi.NewMessage(n.adminChatID, text)
	if priority == "low" {
		msg.DisableNotification = true
	}
	if _, err := n.tgBot.Send(msg); err != nil {
		return fmt.Errorf("telegram admin alert error: %w", err)
	}
	return nil
}

func (n *Notifier) sendSync(title, body, priority, tags string) error {
	if n == nil {
		return fmt.Errorf("notifier not initialized")
	}

	if n.tgBot != nil && n.adminChatID != 0 {
		if err := n.sendTelegram(title, body, priority); err != nil {
			log.Printf("[notify] %v", err)
			n.recordStatus(err.Error())
			return err
		}
		n.recordStatus("Telegram OK")
		log.Printf("[notify] Telegram admin alert sent (%s): %s", priority, title)
		return nil
	}

	if n.ntfyURL == "" {
		return fmt.Errorf("neither ADMIN_CHAT_ID nor NTFY_URL is configured")
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}

		req, err := http.NewRequest(http.MethodPost, n.ntfyURL, strings.NewReader(body))
		if err != nil {
			lastErr = fmt.Errorf("request creation error: %w", err)
			log.Printf("[ntfy] %v", lastErr)
			n.recordStatus(lastErr.Error())
			return lastErr
		}
		if title != "" {
			req.Header.Set("Title", title)
		}
		if priority != "" {
			req.Header.Set("Priority", priority)
		}
		if tags != "" {
			req.Header.Set("Tags", tags)
		}

		resp, err := n.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("delivery error to %s: %w", n.ntfyURL, err)
			log.Printf("[ntfy] %v", lastErr)
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()

		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, n.ntfyURL, strings.TrimSpace(string(respBody)))
			log.Printf("[ntfy] %v", lastErr)
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				continue
			}
			n.recordStatus(lastErr.Error())
			return lastErr
		}

		n.recordStatus("OK (200)")
		log.Printf("[ntfy] notification sent successfully (%s): %s", priority, title)
		return nil
	}

	if lastErr != nil {
		n.recordStatus(lastErr.Error())
	}
	return lastErr
}

func (n *Notifier) send(title, body, priority, tags string) {
	if n == nil || ((n.tgBot == nil || n.adminChatID == 0) && n.ntfyURL == "") {
		return
	}

	go func() {
		_ = n.sendSync(title, body, priority, tags)
	}()
}

// SendTest sends a synchronous test notification and returns any delivery error.
func (n *Notifier) SendTest() error {
	return n.sendSync("Sonar Test Notification", "Test alert from Sonar Admin Dashboard.", "default", "bell,white_check_mark")
}

// NotifyStartup sends an immediate alert when Sonar boots up.
func (n *Notifier) NotifyStartup() {
	n.send("Sonar Bot Online", "🚀 Sonar instance started and ready on Render.", "high", "white_check_mark,rocket")
}

// NotifyUserActivity sends a completely silent activity log ping once per 30-minute user session (no sound, no vibrate).
func (n *Notifier) NotifyUserActivity(chatID int64, username, query string) {
	if n == nil || ((n.tgBot == nil || n.adminChatID == 0) && n.ntfyURL == "") {
		return
	}

	n.mu.Lock()
	last, exists := n.lastChat[chatID]
	if exists && time.Since(last) < n.chatCooldown {
		n.mu.Unlock()
		return
	}
	n.lastChat[chatID] = time.Now()
	n.mu.Unlock()

	body := fmt.Sprintf("%s active on Sonar (Query: %q)", formatUserTag(chatID, username), query)
	// Priority "low" is completely silent in ntfy (no sound, no vibration)
	n.send("Sonar User Active", body, "low", "speech_balloon")
}

// NotifyDownload sends a normal alert when a user initiates a media download (debounced 1 min).
func (n *Notifier) NotifyDownload(chatID int64, username, trackTitle, format string) {
	if n == nil || ((n.tgBot == nil || n.adminChatID == 0) && n.ntfyURL == "") {
		return
	}

	n.mu.Lock()
	last, exists := n.lastDownload[chatID]
	if exists && time.Since(last) < n.dlCooldown {
		n.mu.Unlock()
		return
	}
	n.lastDownload[chatID] = time.Now()
	n.mu.Unlock()

	body := fmt.Sprintf("%s downloading %s: %q", formatUserTag(chatID, username), strings.ToUpper(format), trackTitle)
	n.send("Sonar Download Started", body, "default", "inbox_tray,musical_note")
}

// NotifyPayment always sends an immediate high-priority notification when a pass is purchased.
func (n *Notifier) NotifyPayment(chatID int64, planLabel, currency, amountStr string) {
	body := fmt.Sprintf("Chat %d activated %s (%s %s)", chatID, planLabel, currency, amountStr)
	n.send("Sonar Pass Purchased!", body, "high", "moneybag,tada")
}
