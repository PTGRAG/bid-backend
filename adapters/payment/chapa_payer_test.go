package payment

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPayerAccountFrom(t *testing.T) {
	cases := []struct {
		name string
		data map[string]interface{}
		want string
	}{
		{"none", map[string]interface{}{"method": "telebirr", "email": "a@b.c"}, ""},
		{"mobile", map[string]interface{}{"method": "Telebirr", "mobile": "0911 223 344"}, "telebirr:251911223344"},
		{"nested", map[string]interface{}{"method": "cbebirr", "payment_details": map[string]interface{}{"phone_number": "+251911223344"}}, "cbebirr:251911223344"},
		{"card", map[string]interface{}{"payment_method": "card", "card_number": "4242 **** **** 4242"}, "card:4242********4242"},
		{"no method", map[string]interface{}{"account_number": "1000123456789"}, "unknown:1000123456789"},
	}
	for _, c := range cases {
		if got := payerAccountFrom(c.data); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPayerAccountCallsVerify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transaction/verify/TX-1" || r.Header.Get("Authorization") != "Bearer sk" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"status":"success","method":"telebirr","mobile":"0911223344"}}`))
	}))
	defer srv.Close()
	c := NewChapaClient("sk", srv.URL)
	got, err := c.PayerAccount("TX-1")
	if err != nil || got != "telebirr:251911223344" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := c.PayerAccount("TX-2"); err == nil {
		t.Fatal("a failed verify must return an error so the referral is retried later")
	}
}
