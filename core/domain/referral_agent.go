package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Self-serve agents
//
// Any participant can become an agent from the app: they accept the agent rules and get their
// own code, link and dashboard straight away, with no staff step. The rules that stop one person
// from farming payouts stay the same (one qualified referral per phone and per payment account,
// no self-referrals) and these are added for agents themselves:
//   - one agent code per account;
//   - the account must have a phone number, and no other agent may use it;
//   - each payout account (method + bank + number) belongs to one agent.
// Staff can close self-serve signup at any time and pause any agent.

// Payout methods an agent can choose for being paid.
const (
	PayoutMethodTelebirr = "telebirr"
	PayoutMethodCBEBirr  = "cbe_birr"
	PayoutMethodMPesa    = "mpesa"
	PayoutMethodBank     = "bank"
)

// ValidPayoutMethod reports whether m is a known payout method.
func ValidPayoutMethod(m string) bool {
	switch m {
	case PayoutMethodTelebirr, PayoutMethodCBEBirr, PayoutMethodMPesa, PayoutMethodBank:
		return true
	}
	return false
}

// ReferralPayoutAccount is where an agent wants their earnings sent.
type ReferralPayoutAccount struct {
	Method  string `json:"method"`
	Bank    string `json:"bank,omitempty"`
	Account string `json:"account"`
	Name    string `json:"name"`
}

// Normalize trims the fields and reduces the account to its digits. Mobile money accounts are
// phone numbers, so they get the same international form as PhoneDigits (09.. -> 2519..).
func (p ReferralPayoutAccount) Normalize() ReferralPayoutAccount {
	p.Method = strings.ToLower(strings.TrimSpace(p.Method))
	p.Bank = strings.Join(strings.Fields(p.Bank), " ")
	p.Name = strings.Join(strings.Fields(p.Name), " ")
	if p.Method == PayoutMethodBank {
		var b strings.Builder
		for _, r := range p.Account {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		p.Account = b.String()
	} else {
		p.Account = PhoneDigits(p.Account)
		p.Bank = ""
	}
	return p
}

// Validate checks a normalized payout account.
func (p ReferralPayoutAccount) Validate() error {
	if !ValidPayoutMethod(p.Method) {
		return NewAppError(400, "Choose how you want to be paid", ErrInvalidInput)
	}
	if p.Method == PayoutMethodBank {
		if len(p.Bank) < 2 || len(p.Bank) > 60 {
			return NewAppError(400, "Enter your bank's name", ErrInvalidInput)
		}
		if len(p.Account) < 6 || len(p.Account) > 30 {
			return NewAppError(400, "Enter a valid bank account number", ErrInvalidInput)
		}
	} else if len(p.Account) != 12 || !strings.HasPrefix(p.Account, "251") {
		return NewAppError(400, "Enter a valid Ethiopian phone number for mobile money", ErrInvalidInput)
	}
	if len(p.Name) < 2 || len(p.Name) > 120 {
		return NewAppError(400, "Enter the name on the account", ErrInvalidInput)
	}
	return nil
}

// ReferralSettings is the one-row staff switchboard for the referral program.
type ReferralSettings struct {
	ID int `gorm:"primaryKey" json:"-"`
	// AgentSignupOpen lets participants become agents themselves. When false, the app explains
	// that new agents are paused; existing agents keep working.
	AgentSignupOpen bool       `gorm:"not null;default:true" json:"agentSignupOpen"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	UpdatedBy       *uuid.UUID `gorm:"type:uuid" json:"-"`
}

func (ReferralSettings) TableName() string { return "referral_settings" }
