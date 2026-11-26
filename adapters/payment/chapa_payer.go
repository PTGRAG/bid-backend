package payment

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
)

// payerAccountKeys are the verify-response fields that may identify the paying wallet, bank
// account or card, most specific first. Chapa's documented verify response does not promise any
// of them, so PayerAccount returns "" when none is present and the referral rule then relies on
// the phone number alone.
var payerAccountKeys = []string{"payer_account", "account_number", "mobile", "phone_number", "phone", "card_number", "last4"}

// PayerAccount implements ports.PayerAccountResolver: it verifies txRef with Chapa and returns
// "<method>:<account>" (for example "telebirr:251911223344"), or "" when Chapa does not say who
// paid.
func (c *ChapaClient) PayerAccount(txRef string) (string, error) {
	txRef = strings.TrimSpace(txRef)
	if txRef == "" {
		return "", fmt.Errorf("empty tx_ref")
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/transaction/verify/"+url.PathEscape(txRef), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("chapa verify request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chapa verify api error (status %d)", resp.StatusCode)
	}
	var vr struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &vr); err != nil {
		return "", fmt.Errorf("decode verify response: %w", err)
	}
	return payerAccountFrom(vr.Data), nil
}

func payerAccountFrom(data map[string]interface{}) string {
	if data == nil {
		return ""
	}
	method := strings.ToLower(firstString(data, "method", "payment_method", "type"))
	sources := []map[string]interface{}{data}
	for _, k := range []string{"payment_details", "payer", "customer"} {
		if m, ok := data[k].(map[string]interface{}); ok {
			sources = append(sources, m)
		}
	}
	for _, src := range sources {
		id := firstString(src, payerAccountKeys...)
		if id == "" {
			continue
		}
		if d := domain.PhoneDigits(id); len(d) >= 9 && len(d) <= 15 && !strings.ContainsAny(id, "*xX") {
			id = d
		} else {
			id = strings.ToLower(strings.Join(strings.Fields(id), ""))
		}
		if method == "" {
			method = "unknown"
		}
		return method + ":" + id
	}
	return ""
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}
