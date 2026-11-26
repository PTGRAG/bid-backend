package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestReferralPayoutAccount_NormalizeValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      ReferralPayoutAccount
		account string
		ok      bool
	}{
		{"telebirr local", ReferralPayoutAccount{Method: "Telebirr", Account: "0911 22 33 44", Name: " Abebe  Kebede "}, "251911223344", true},
		{"telebirr short", ReferralPayoutAccount{Method: "telebirr", Account: "911223344", Name: "Abebe"}, "251911223344", true},
		{"mpesa safaricom", ReferralPayoutAccount{Method: "mpesa", Account: "+251 711 223 344", Name: "Abebe"}, "251711223344", true},
		{"wallet bad number", ReferralPayoutAccount{Method: "cbe_birr", Account: "12345", Name: "Abebe"}, "12345", false},
		{"bank", ReferralPayoutAccount{Method: "bank", Bank: "Commercial Bank of Ethiopia", Account: "1000-1234-5678", Name: "Abebe Kebede"}, "100012345678", true},
		{"bank without name", ReferralPayoutAccount{Method: "bank", Account: "100012345678", Name: "Abebe"}, "100012345678", false},
		{"unknown method", ReferralPayoutAccount{Method: "paypal", Account: "0911223344", Name: "Abebe"}, "251911223344", false},
		{"no holder name", ReferralPayoutAccount{Method: "telebirr", Account: "0911223344"}, "251911223344", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.in.Normalize()
			if p.Account != c.account {
				t.Fatalf("account = %q, want %q", p.Account, c.account)
			}
			if p.Method != PayoutMethodBank && p.Bank != "" {
				t.Fatalf("bank kept for wallet: %q", p.Bank)
			}
			if err := p.Validate(); (err == nil) != c.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, c.ok)
			}
		})
	}
	if got := (ReferralPayoutAccount{Method: "telebirr", Account: "0911223344", Name: " Abebe  Kebede "}).Normalize().Name; got != "Abebe Kebede" {
		t.Fatalf("name = %q", got)
	}
}

func TestReferralCode_SelfSignup(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if !(&ReferralCode{OwnerUserID: &a, CreatedBy: &a}).SelfSignup() {
		t.Fatal("owner-created code should be self signup")
	}
	if (&ReferralCode{OwnerUserID: &a, CreatedBy: &b}).SelfSignup() {
		t.Fatal("staff-created code is not self signup")
	}
	if (&ReferralCode{CreatedBy: &a}).SelfSignup() {
		t.Fatal("code without owner is not self signup")
	}
}
