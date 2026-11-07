package bot

import (
	"fmt"
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
		cooldown:     6 * time.Hour, // At most 1 usage alert per user every 6 hours (anti-spam)
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

// NotifyUserActivity sends at most one notification per user per 6-hour window so you never get spammed.
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

	go func() {
		body := fmt.Sprintf("%s is using Sonar (Query: %q)", formatUserTag(chatID, username), query)
		req, err := http.NewRequest(http.MethodPost, n.ntfyURL, strings.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Title", "Sonar Bot Active")
		req.Header.Set("Priority", "default")
		req.Header.Set("Tags", "headphones")
		resp, err := n.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

// NotifyPayment always sends an immediate notification when a user purchases a pass.
func (n *Notifier) NotifyPayment(chatID int64, planLabel, currency, amountStr string) {
	if n == nil || n.ntfyURL == "" {
		return
	}

	go func() {
		body := fmt.Sprintf("Chat %d activated %s (%s %s)", chatID, planLabel, currency, amountStr)
		req, err := http.NewRequest(http.MethodPost, n.ntfyURL, strings.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Title", "Sonar Pass Purchased!")
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "moneybag,tada")
		resp, err := n.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

