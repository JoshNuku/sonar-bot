package config

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	TelegramBotToken string
	TelegramSecret   string
	GroqAPIKey       string
	GroqModel        string
	PaystackSecret   string
	PaystackCurrency string
	TierDayPrice     int
	TierWeeklyPrice  int
	TierMonthlyPrice int
	DailyFreeLimit   int
	DatabaseURL      string
	PublicURL        string
	Port             string
	NtfyURL          string
}

func loadDotEnv(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		return // Silently ignore if .env file does not exist
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Strip inline comments if value is unquoted, or extract quoted value
		if strings.HasPrefix(val, `"`) || strings.HasPrefix(val, `'`) {
			quote := val[0]
			if endIdx := strings.IndexByte(val[1:], quote); endIdx != -1 {
				val = val[1 : endIdx+1]
			} else {
				val = strings.Trim(val, `"'`)
			}
		} else if idx := strings.Index(val, "#"); idx != -1 {
			val = strings.TrimSpace(val[:idx])
		}

		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func parseEnvInt(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		log.Fatalf("FATAL: invalid %s: %q", key, raw)
	}
	return parsed
}

// sanitizeWebhookSecret ensures the secret only contains Telegram-allowed characters (A-Z, a-z, 0-9, _, -).
func sanitizeWebhookSecret(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	res := b.String()
	if len(res) > 256 {
		res = res[:256]
	}
	return res
}

func LoadConfig() Config {
	loadDotEnv(".env")

	currency := os.Getenv("PAYSTACK_CURRENCY")
	if currency == "" {
		currency = "GHS"
	}

	groqModel := os.Getenv("GROQ_MODEL")
	if groqModel == "" {
		groqModel = "openai/gpt-oss-20b"
	}

	cfg := Config{
		TelegramBotToken: strings.Trim(strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")), `"'`),
		TelegramSecret:   sanitizeWebhookSecret(os.Getenv("TELEGRAM_WEBHOOK_SECRET")),
		GroqAPIKey:       strings.Trim(strings.TrimSpace(os.Getenv("GROQ_API_KEY")), `"'`),
		GroqModel:        groqModel,
		PaystackSecret:   strings.Trim(strings.TrimSpace(os.Getenv("PAYSTACK_SECRET_KEY")), `"'`),
		PaystackCurrency: currency,
		TierDayPrice:     parseEnvInt("TIER_DAY_PRICE", 300),
		TierWeeklyPrice:  parseEnvInt("TIER_WEEKLY_PRICE", 700),
		TierMonthlyPrice: parseEnvInt("TIER_MONTHLY_PRICE", 1200),
		DailyFreeLimit:   parseEnvInt("DAILY_FREE_LIMIT", 5),
		DatabaseURL:      strings.Trim(strings.TrimSpace(os.Getenv("DATABASE_URL")), `"'`),
		PublicURL:        strings.TrimRight(strings.Trim(strings.TrimSpace(os.Getenv("PUBLIC_URL")), `"'`), "/"),
		Port:             strings.Trim(strings.TrimSpace(os.Getenv("PORT")), `"'`),
		NtfyURL:          strings.Trim(strings.TrimSpace(os.Getenv("NTFY_URL")), `"'`),
	}

	if cfg.NtfyURL == "" {
		cfg.NtfyURL = "https://ntfy.sh/sonar-bot-119"
	}

	if cfg.TelegramBotToken == "" || cfg.GroqAPIKey == "" || cfg.DatabaseURL == "" {
		log.Fatal("FATAL: TELEGRAM_BOT_TOKEN, GROQ_API_KEY, and DATABASE_URL must be defined")
	}

	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	return cfg
}
