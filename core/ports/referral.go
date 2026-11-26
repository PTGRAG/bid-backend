package ports

import (
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// ReferralCodeFilter narrows the staff list of codes.
type ReferralCodeFilter struct {
	Kind   string
	Status string
	Search string // code, label, owner name or phone
	Page   int
	Limit  int
}

// ReferralCodeRow is a code with its owner and counters, for staff lists.
type ReferralCodeRow struct {
	domain.ReferralCode
	OwnerName  string `json:"ownerName"`
	OwnerPhone string `json:"ownerPhone"`
	ReferralCounts
}

// ReferralCounts are the funnel counters of one code.
type ReferralCounts struct {
	Visitors  int64        `json:"visitors"`  // unique visitors who opened the link
	Visits    int64        `json:"visits"`    // total link opens
	Signups   int64        `json:"signups"`   // attributed accounts, any status
	Pending   int64        `json:"pending"`   // signed up, no qualifying paid ticket yet
	Qualified int64        `json:"qualified"` // qualified bidders
	Rejected  int64        `json:"rejected"`  // never count (fraud rules or staff)
	PaidOut   domain.Money `json:"paidOut"`
}

// ReferralAttributionFilter narrows a code's list of referred accounts.
type ReferralAttributionFilter struct {
	Status string
	Page   int
	Limit  int
}

// ReferralAttributionRow is an attribution with the referred account's details.
type ReferralAttributionRow struct {
	domain.ReferralAttribution
	Code          string    `json:"code"`
	UserName      string    `json:"-"`
	UserPhone     string    `json:"-"`
	UserCreatedAt time.Time `json:"-"`
	AuctionTitle  string    `json:"-"`
	// SameDevice counts other accounts attributed from the same browser (visitor id), a hint
	// that an agent signed people up on one phone. Not a rule: assisted signups are allowed.
	SameDevice int64 `json:"-"`
}

// ReferralCandidate is a signed-up attribution being checked for qualification.
type ReferralCandidate struct {
	AttributionID  uuid.UUID
	CodeID         uuid.UUID
	ReferredUserID uuid.UUID
	SignedUpAt     time.Time
	UserPhone      string
	OwnerUserID    *uuid.UUID
	OwnerPhone     string
	Kind           string
	ProgramID      *uuid.UUID
}

// ReferralQualifyingPayment is the first paid ticket that qualifies a referral.
type ReferralQualifyingPayment struct {
	TxRef     string
	AuctionID uuid.UUID
	CreatedAt time.Time
}

// ReferralDailyCount is one day of a code's funnel.
type ReferralDailyCount struct {
	Day       string `json:"day"` // YYYY-MM-DD (Africa/Addis_Ababa)
	Visitors  int64  `json:"visitors"`
	Signups   int64  `json:"signups"`
	Qualified int64  `json:"qualified"`
}

// ReferralRepository persists referral programs, codes, clicks, attributions and payouts.
type ReferralRepository interface {
	// Programs
	ListPrograms() ([]domain.ReferralProgram, error)
	GetProgram(id uuid.UUID) (*domain.ReferralProgram, error)
	GetDefaultProgram(kind string) (*domain.ReferralProgram, error)
	CreateProgram(p *domain.ReferralProgram) error
	UpdateProgram(p *domain.ReferralProgram) error
	AuctionTitles(ids []uuid.UUID) (map[uuid.UUID]string, error)
	LowestEntranceFee(ids []uuid.UUID) (*domain.Money, error)

	// Codes
	CreateCode(c *domain.ReferralCode) error
	UpdateCode(c *domain.ReferralCode) error
	GetCode(id uuid.UUID) (*domain.ReferralCode, error)
	GetCodeByCode(code string) (*domain.ReferralCode, error)
	ListCodesByOwner(ownerID uuid.UUID) ([]domain.ReferralCode, error)
	ListCodes(f ReferralCodeFilter) ([]ReferralCodeRow, int64, error)
	Counts(codeIDs []uuid.UUID) (map[uuid.UUID]ReferralCounts, error)
	Activity(codeIDs []uuid.UUID, since time.Time) ([]ReferralDailyCount, error)

	// Clicks
	UpsertClick(c *domain.ReferralClick) error

	// Attributions
	CreateAttribution(a *domain.ReferralAttribution) error
	GetAttribution(id uuid.UUID) (*domain.ReferralAttribution, error)
	GetAttributionByUser(userID uuid.UUID) (*domain.ReferralAttribution, error)
	ListAttributions(codeIDs []uuid.UUID, f ReferralAttributionFilter) ([]ReferralAttributionRow, int64, error)
	PendingCandidates(limit int) ([]ReferralCandidate, error)
	Candidate(attributionID uuid.UUID) (*ReferralCandidate, error)
	FirstQualifyingPayment(userID uuid.UUID, from, to time.Time, auctionIDs []uuid.UUID) (*ReferralQualifyingPayment, error)
	HasPaidEntry(userID uuid.UUID) (bool, error)
	PhoneQualified(phone string) (bool, error)
	AccountQualified(account string) (bool, error)
	MarkQualified(id uuid.UUID, pay ReferralQualifyingPayment, phone, account string, at time.Time) error
	MarkRejected(id uuid.UUID, reason, note string) error
	MarkSignedUp(id uuid.UUID) error
	// LapsedQualified returns qualified attributions whose qualifying ticket was refunded or failed.
	LapsedQualified(limit int) ([]uuid.UUID, error)
	// WithSweepLock runs fn while holding the referral sweep lock; ok=false when another sweep
	// holds it (fn is not run).
	WithSweepLock(fn func(repo ReferralRepository) error) (ok bool, err error)

	// Payouts
	CreatePayout(p *domain.ReferralPayout) error
	ListPayouts(codeIDs []uuid.UUID) ([]domain.ReferralPayout, error)
}

// PayerAccountResolver returns a stable identifier for the account that paid a transaction
// (payment method plus wallet or card), or "" when the provider does not say.
type PayerAccountResolver interface {
	PayerAccount(txRef string) (string, error)
}
