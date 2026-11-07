package payment

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type PlanTier string

const (
	TierDay     PlanTier = "day"
	TierWeekly  PlanTier = "weekly"
	TierMonthly PlanTier = "monthly"
)

type PlanInfo struct {
	Tier     PlanTier
	Label    string
	Amount   int
	Duration time.Duration
}

// FlexInt64 unmarshals both JSON numbers (2081516511) and JSON strings ("2081516511"),
// because Paystack converts metadata integer values into strings in webhook and verify responses.
type FlexInt64 int64

func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	*f = FlexInt64(v)
	return nil
}

type PaystackService struct {
	secretKey string
	currency  string
	plans     map[PlanTier]PlanInfo
	client    *http.Client
}

func NewPaystackService(secretKey, currency string, dayPrice, weeklyPrice, monthlyPrice int) *PaystackService {
	if currency == "" {
		currency = "GHS"
	}
	if dayPrice <= 0 {
		dayPrice = 300
	}
	if weeklyPrice <= 0 {
		weeklyPrice = 700
	}
	if monthlyPrice <= 0 {
		monthlyPrice = 1200
	}

	plans := map[PlanTier]PlanInfo{
		TierDay: {
			Tier:     TierDay,
			Label:    "Day Pass (24 Hours)",
			Amount:   dayPrice,
			Duration: 24 * time.Hour,
		},
		TierWeekly: {
			Tier:     TierWeekly,
			Label:    "Weekly Pass (7 Days)",
			Amount:   weeklyPrice,
			Duration: 7 * 24 * time.Hour,
		},
		TierMonthly: {
			Tier:     TierMonthly,
			Label:    "Monthly Unlimited (30 Days)",
			Amount:   monthlyPrice,
			Duration: 30 * 24 * time.Hour,
		},
	}

	return &PaystackService{
		secretKey: secretKey,
		currency:  currency,
		plans:     plans,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *PaystackService) Currency() string {
	return p.currency
}

func (p *PaystackService) GetPlan(tier PlanTier) (PlanInfo, bool) {
	plan, ok := p.plans[tier]
	return plan, ok
}

func (p *PaystackService) ResolvePlan(tierStr string, paidAmount int) PlanInfo {
	if plan, ok := p.plans[PlanTier(tierStr)]; ok {
		return plan
	}
	for _, plan := range p.plans {
		if plan.Amount == paidAmount {
			return plan
		}
	}
	return p.plans[TierDay]
}

func FormatSubunits(amount int) string {
	if amount%100 == 0 {
		return fmt.Sprintf("%d", amount/100)
	}
	return fmt.Sprintf("%.2f", float64(amount)/100.0)
}

type InitResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		AuthorizationURL string `json:"authorization_url"`
		Reference        string `json:"reference"`
	} `json:"data"`
}

type TransactionData struct {
	Status    string `json:"status"`
	Reference string `json:"reference"`
	Amount    int    `json:"amount"`
	Customer  struct {
		Email string `json:"email"`
	} `json:"customer"`
	Metadata struct {
		TelegramChatID FlexInt64 `json:"telegram_chat_id"`
		PlanTier       string    `json:"plan_tier"`
	} `json:"metadata"`
}

type WebhookPayload struct {
	Event string          `json:"event"`
	Data  TransactionData `json:"data"`
}

type VerifyResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    TransactionData `json:"data"`
}

func (p *PaystackService) CreateMoMoLink(chatID int64, username string, tier PlanTier) (string, string, error) {
	plan, ok := p.plans[tier]
	if !ok {
		return "", "", fmt.Errorf("invalid plan tier: %s", tier)
	}

	url := "https://api.paystack.co/transaction/initialize"
	payload := map[string]interface{}{
		"email":    fmt.Sprintf("user_%d@sonarapp.net", chatID),
		"amount":   plan.Amount,
		"currency": p.currency,
		"channels": []string{"mobile_money"},
		"metadata": map[string]interface{}{
			"telegram_chat_id": chatID,
			"username":         username,
			"plan_tier":        string(plan.Tier),
		},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", "", err
	}

	req.Header.Set("Authorization", "Bearer "+p.secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var result InitResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}
	if !result.Status {
		return "", "", fmt.Errorf("paystack rejected: %s", result.Message)
	}

	return result.Data.AuthorizationURL, result.Data.Reference, nil
}

// VerifyTransaction checks a specific reference directly against Paystack's API.
func (p *PaystackService) VerifyTransaction(reference string) (*TransactionData, error) {
	url := fmt.Sprintf("https://api.paystack.co/transaction/verify/%s", reference)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.secretKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result VerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.Status || result.Data.Status != "success" {
		return nil, fmt.Errorf("transaction not successful yet (status: %s)", result.Data.Status)
	}
	return &result.Data, nil
}

// RecentSonarTransactions returns all recent successful Sonar transactions from Paystack.
func (p *PaystackService) RecentSonarTransactions() ([]TransactionData, error) {
	url := "https://api.paystack.co/transaction?perPage=15&status=success"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.secretKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rawList struct {
		Status bool              `json:"status"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawList); err != nil {
		return nil, err
	}

	var results []TransactionData
	for _, item := range rawList.Data {
		var tx TransactionData
		if err := json.Unmarshal(item, &tx); err != nil {
			continue
		}
		if tx.Status != "success" || tx.Reference == "" {
			continue
		}
		if tx.Metadata.TelegramChatID != 0 && strings.HasSuffix(strings.ToLower(tx.Customer.Email), "@sonarapp.net") {
			results = append(results, tx)
		}
	}
	return results, nil
}

func (p *PaystackService) VerifySignature(rawBody []byte, signatureHeader string) bool {
	mac := hmac.New(sha512.New, []byte(p.secretKey))
	mac.Write(rawBody)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signatureHeader), []byte(expectedSig))
}
