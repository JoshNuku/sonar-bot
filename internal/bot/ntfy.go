package bot

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Notifier struct {
	ntfyURL      string
	cooldown     time.Duration
	mu           sync.Mutex
	lastNotified map[int64]time.Time
	client       *http.Client
}

func NewNotifier(ntfyURL string) *Notifier {
	return &Notifier{
		ntfyURL:      strings.TrimSpace(ntfyURL),
		cooldown:     15 * time.Second, // Debounce rapid spam per user
		lastNotified: make(map[int64]time.Time),
		client:       &http.Client{Timeout: 8 * time.Second},
	}
}

func formatUserTag(chatID int64, username string) string {
	if username != "" {
		return fmt.Sprintf("@%s (%d)", username, chatID)
	}
	return fmt.Sprintf("User %d", chatID)
}

func (n *Notifier) send(title, body, priority, tags string) {
	if n == nil || n.ntfyURL == "" {
		return
	}

	go func() {
		req, err := http.NewRequest(http.MethodPost, n.ntfyURL, strings.NewReader(body))
		if err != nil {
			log.Printf("[ntfy] request creation error: %v", err)
			return
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
			log.Printf("[ntfy] delivery error to %s: %v", n.ntfyURL, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 300 {
			log.Printf("[ntfy] unexpected response from %s: %d", n.ntfyURL, resp.StatusCode)
		} else {
			log.Printf("[ntfy] notification sent successfully: %s", title)
		}
	}()
}

// NotifyStartup sends an immediate alert when Sonar boots up.
func (n *Notifier) NotifyStartup() {
	n.send("Sonar Bot Online", "🚀 Sonar instance started and ready on Render.", "high", "white_check_mark,rocket")
}

// NotifyUserActivity sends an alert when a user sends a query or chat message.
func (n *Notifier) NotifyUserActivity(chatID int64, username, query string) {
	if n == nil || n.ntfyURL == "" {
		return
	}

	n.mu.Lock()
	last, exists := n.lastNotified[chatID]
	if exists && time.Since(last) < n.cooldown {
		n.mu.Unlock()
		return
	}
	n.lastNotified[chatID] = time.Now()
	n.mu.Unlock()

	body := fmt.Sprintf("%s is using Sonar (Query: %q)", formatUserTag(chatID, username), query)
	n.send("Sonar Bot Active", body, "default", "headphones")
}

// NotifyDownload sends an alert when a user initiates a media download.
func (n *Notifier) NotifyDownload(chatID int64, username, trackTitle, format string) {
	body := fmt.Sprintf("%s downloading %s: %q", formatUserTag(chatID, username), strings.ToUpper(format), trackTitle)
	n.send("Sonar Download Started", body, "default", "inbox_tray,musical_note")
}

// NotifyPayment always sends an immediate high-priority notification when a pass is purchased.
func (n *Notifier) NotifyPayment(chatID int64, planLabel, currency, amountStr string) {
	body := fmt.Sprintf("Chat %d activated %s (%s %s)", chatID, planLabel, currency, amountStr)
	n.send("Sonar Pass Purchased!", body, "high", "moneybag,tada")
}
