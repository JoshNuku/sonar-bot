package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"sonar/internal/ai"
	"sonar/internal/bot"
	"sonar/internal/config"
	"sonar/internal/dashboard"
	"sonar/internal/database"
	"sonar/internal/media"
	"sonar/internal/music"
	"sonar/internal/payment"
)

func main() {
	cfg := config.LoadConfig()

	db, err := database.NewPool(cfg.DatabaseURL, cfg.DailyFreeLimit)
	if err != nil {
		log.Fatalf("Database pool error: %v", err)
	}
	defer db.Close()

	tgBot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		log.Fatalf("Telegram API error: %v", err)
	}
	tgBot.Client = &http.Client{
		Timeout: 20 * time.Second,
	}

	groq := ai.NewGroqClient(cfg.GroqAPIKey, cfg.GroqModel)
	dl := media.NewDownloader(3) // 3 concurrent downloads
	deezer := music.NewDeezerClient()
	ps := payment.NewPaystackService(
		cfg.PaystackSecret,
		cfg.PaystackCurrency,
		cfg.TierDayPrice,
		cfg.TierWeeklyPrice,
		cfg.TierMonthlyPrice,
	)

	controller := bot.NewBotController(tgBot, db, groq, dl, ps, deezer, cfg.NtfyURL, cfg.AdminChatID)
	controller.NotifyStartup()
	go controller.StartPaymentWatcher()

	// Register Admin Dashboard
	adminServer := dashboard.NewServer(db, cfg.AdminPassword, cfg.TelegramBotToken, deezer, controller)
	adminServer.RegisterRoutes(http.DefaultServeMux)

	// Auxiliary HTTP Handlers
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	http.HandleFunc("/paystack/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sig := r.Header.Get("X-Paystack-Signature")
		if !ps.VerifySignature(body, sig) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var event payment.WebhookPayload
		if err := json.Unmarshal(body, &event); err == nil && event.Event == "charge.success" && event.Data.Status == "success" {
			ctx := r.Context()

			tier := payment.PlanTier(event.Data.Metadata.PlanTier)
			plan, ok := ps.GetPlan(tier)
			if !ok {
				// Fallback by matching paid amount if metadata tier was omitted
				switch event.Data.Amount {
				case cfg.TierDayPrice:
					plan, _ = ps.GetPlan(payment.TierDay)
				case cfg.TierWeeklyPrice:
					plan, _ = ps.GetPlan(payment.TierWeekly)
				default:
					plan, _ = ps.GetPlan(payment.TierMonthly)
				}
			}

			paidAmount := event.Data.Amount
			if paidAmount <= 0 {
				paidAmount = plan.Amount
			}

			chatID := int64(event.Data.Metadata.TelegramChatID)
			expiresAt, err := db.ProcessPaymentSuccess(
				ctx,
				event.Data.Reference,
				chatID,
				paidAmount,
				string(plan.Tier),
				plan.Duration,
			)
			if err == nil && !expiresAt.IsZero() {
				msg := fmt.Sprintf(
					"🎉 **%s Activated!**\n\nUnlimited Sonar downloads unlocked until **%s UTC**.",
					plan.Label,
					expiresAt.UTC().Format("02 Jan 2006 15:04"),
				)
				reply := tgbotapi.NewMessage(chatID, msg)
				reply.ParseMode = "Markdown"
				tgBot.Send(reply)
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	// Webhook Mode (Production)
	if cfg.PublicURL != "" {
		whURL := fmt.Sprintf("%s/tg/webhook", cfg.PublicURL)
		params := tgbotapi.Params{"url": whURL}
		if cfg.TelegramSecret != "" {
			params["secret_token"] = cfg.TelegramSecret
		}
		if _, err := tgBot.MakeRequest("setWebhook", params); err != nil {
			log.Fatalf("Failed to bind webhook: %v", err)
		}

		http.HandleFunc("/tg/webhook", func(w http.ResponseWriter, r *http.Request) {
			if cfg.TelegramSecret != "" && r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != cfg.TelegramSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var update tgbotapi.Update
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)

			if update.Message != nil && update.Message.Text != "" {
				go controller.HandleMessage(update.Message)
			} else if update.EditedMessage != nil && update.EditedMessage.Text != "" {
				go controller.HandleEditedMessage(update.EditedMessage)
			} else if update.CallbackQuery != nil {
				go controller.HandleCallback(update.CallbackQuery)
			}
		})

		log.Printf("Sonar running via Webhook on %s (Port :%s)", whURL, cfg.Port)
		log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
	}

	// Long Polling Mode (Local Testing)
	go func() {
		log.Printf("Listening on :%s for /health and /paystack/webhook", cfg.Port)
		if err := http.ListenAndServe(":"+cfg.Port, nil); err != nil {
			log.Printf("HTTP server exited: %v", err)
		}
	}()

	_, _ = tgBot.Request(tgbotapi.DeleteWebhookConfig{})
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 10
	updates := tgBot.GetUpdatesChan(u)

	log.Printf("Sonar active on @%s via Long Polling", tgBot.Self.UserName)
	for update := range updates {
		if update.Message != nil && update.Message.Text != "" {
			go controller.HandleMessage(update.Message)
		} else if update.EditedMessage != nil && update.EditedMessage.Text != "" {
			go controller.HandleEditedMessage(update.EditedMessage)
		} else if update.CallbackQuery != nil {
			go controller.HandleCallback(update.CallbackQuery)
		}
	}
}
