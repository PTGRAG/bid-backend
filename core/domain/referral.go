package domain

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Referrals
//
// A referral code belongs to an agent (Phase 1) or an influencer (Phase 2). People who open the
// code's link are counted as clicks; the ones who then sign up are attributed to the code; an
// attributed signup becomes a QUALIFIED BIDDER once they buy a paid ticket.
//
// Qualified bidder (pay-only-if-you-win): bids are never charged at checkout, only the entrance
// fee per ticket is, and only the winner later pays their bid amount. So "paid bid" means a
// successful ticket checkout that charged real money (payments.purpose = 'entry', status =
// 'success', amount > 0) on one of the program's qualifying auctions (or any auction when the
// program lists none), made within the program's attribution window after signup. A free ticket
// (free-entrance promo or a free bid credit) costs 0 and never qualifies, and the winner payment
// is not required, since only one person per auction can ever make it.
//
// Anti-fraud: a referral counts only once per phone number and once per payment account, across
// every code and kind. A referral whose phone matches the code owner's is a self-referral.

// Referral kinds. Influencer codes reuse the same tables, flow and payout engine.
const (
	ReferralKindAgent      = "agent"
	ReferralKindInfluencer = "influencer"
)

// Referral code statuses. Paused codes keep their history but stop taking clicks and signups.
const (
	ReferralCodeActive = "active"
	ReferralCodePaused = "paused"
)

// Referral attribution statuses.
const (
	ReferralSignedUp  = "signed_up" // attributed signup, no qualifying paid ticket yet
	ReferralQualified = "qualified" // counts toward the threshold and earnings
	ReferralRejected  = "rejected"  // never counts (see reject reasons)
)

// Reject reasons (stored on the attribution; shown to the agent in plain words by the UI).
const (
	ReferralRejectSelf             = "self_referral"
	ReferralRejectDuplicatePhone   = "duplicate_phone"
	ReferralRejectDuplicateAccount = "duplicate_payment_account"
	ReferralRejectManual           = "rejected_by_staff"
)

const (
	// ReferralClaimWindow is how soon after creating their account a person must attach a code.
	// It stops existing customers from being "claimed" by an agent later.
	ReferralClaimWindow = 72 * time.Hour

	DefaultReferralThreshold               = 10
	DefaultReferralAttributionDays         = 30
	MaxReferralAttributionDays             = 365
	MaxReferralThreshold                   = 100000
	referralGeneratedCodeLength            = 6
	referralGeneratedCodeAlphabet          = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I
	referralCodeMinLen, referralCodeMaxLen = 4, 20
)

// DefaultNetMarginPerBid is the starting guardrail: what Carra nets from one paid ticket after
// payment fees and prize cost. Staff should set the real figure in the program settings.
var DefaultNetMarginPerBid = NewMoney(25)

// DefaultReferralTiers: nothing for the first 10 qualified bidders (the threshold), then the
// per-bidder payout doubles at each step: 5 ETB for bidders 11-25, 10 ETB for 26-50, 20 ETB
// from 51 on. The top tier stays below DefaultNetMarginPerBid.
func DefaultReferralTiers() []ReferralTier {
	to25, to50 := 25, 50
	return []ReferralTier{
		{From: 11, To: &to25, Amount: NewMoney(5)},
		{From: 26, To: &to50, Amount: NewMoney(10)},
		{From: 51, Amount: NewMoney(20)},
	}
}

// ReferralTier pays Amount for every qualified bidder numbered From..To (inclusive); To nil
// means "and every bidder after that". Bidders are numbered 1, 2, 3, ... in the order they
// qualified.
type ReferralTier struct {
	From   int   `json:"from"`
	To     *int  `json:"to,omitempty"`
	Amount Money `json:"amount"`
}

// ReferralProgram holds the payout rules for a kind of code. Each kind has one default program;
// a code may point at another program to run a different deal.
type ReferralProgram struct {
	ID                    uuid.UUID      `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	Name                  string         `gorm:"type:varchar(120);not null" json:"name"`
	Kind                  string         `gorm:"type:varchar(20);not null" json:"kind"`
	IsDefault             bool           `gorm:"not null;default:false" json:"isDefault"`
	Threshold             int            `gorm:"not null" json:"threshold"`
	Tiers                 []ReferralTier `gorm:"type:jsonb;serializer:json;not null" json:"tiers"`
	NetMarginPerBid       Money          `gorm:"type:decimal(12,2);not null" json:"netMarginPerBid"`
	QualifyingAuctionIDs  []uuid.UUID    `gorm:"type:jsonb;serializer:json;not null;column:qualifying_auction_ids" json:"qualifyingAuctionIds"`
	AttributionWindowDays int            `gorm:"not null" json:"attributionWindowDays"`
	CreatedAt             time.Time      `json:"createdAt"`
	UpdatedAt             time.Time      `json:"updatedAt"`
}

func (ReferralProgram) TableName() string { return "referral_programs" }

// NewDefaultReferralProgram returns the built-in defaults for a kind.
func NewDefaultReferralProgram(kind string) ReferralProgram {
	name := "Agent program"
	if kind == ReferralKindInfluencer {
		name = "Influencer program"
	}
	return ReferralProgram{
		Name:                  name,
		Kind:                  kind,
		IsDefault:             true,
		Threshold:             DefaultReferralThreshold,
		Tiers:                 DefaultReferralTiers(),
		NetMarginPerBid:       DefaultNetMarginPerBid,
		QualifyingAuctionIDs:  []uuid.UUID{},
		AttributionWindowDays: DefaultReferralAttributionDays,
	}
}

// ValidReferralKind reports whether kind is a known code kind.
func ValidReferralKind(kind string) bool {
	return kind == ReferralKindAgent || kind == ReferralKindInfluencer
}

// Validate checks the payout rules. Tiers must start right after the threshold, run on without
// gaps or overlaps, never pay less per bidder than the tier before, end open-ended, and (the
// guardrail) the top tier must pay strictly less per bidder than the net margin per bid, so every
// referral stays profitable even if that bidder buys only one ticket.
func (p *ReferralProgram) Validate() error {
	bad := func(format string, a ...interface{}) error {
		return NewAppError(400, fmt.Sprintf(format, a...), ErrInvalidInput)
	}
	if strings.TrimSpace(p.Name) == "" {
		return bad("Program name is required")
	}
	if !ValidReferralKind(p.Kind) {
		return bad("Kind must be %q or %q", ReferralKindAgent, ReferralKindInfluencer)
	}
	if p.Threshold < 0 || p.Threshold > MaxReferralThreshold {
		return bad("Threshold must be between 0 and %d qualified bidders", MaxReferralThreshold)
	}
	if p.AttributionWindowDays < 1 || p.AttributionWindowDays > MaxReferralAttributionDays {
		return bad("Attribution window must be between 1 and %d days", MaxReferralAttributionDays)
	}
	if !p.NetMarginPerBid.IsPositive() {
		return bad("Net margin per bid must be more than 0 ETB")
	}
	if len(p.Tiers) == 0 {
		return bad("Add at least one payout tier")
	}
	next := p.Threshold + 1
	prev := Zero()
	for i, t := range p.Tiers {
		n := i + 1
		if t.From != next {
			return bad("Tier %d must start at bidder %d (tiers start right after the threshold and leave no gaps)", n, next)
		}
		if t.Amount.IsNegative() {
			return bad("Tier %d pays a negative amount", n)
		}
		if t.Amount.LessThan(prev) {
			return bad("Tier %d pays less per bidder than the tier before it; payouts may only rise", n)
		}
		if !t.Amount.LessThan(p.NetMarginPerBid) {
			return bad("Tier %d pays %s ETB per bidder, which is not below the net margin per bid (%s ETB)", n, t.Amount.String(), p.NetMarginPerBid.String())
		}
		last := i == len(p.Tiers)-1
		if t.To == nil {
			if !last {
				return bad("Only the last tier may be open-ended")
			}
		} else {
			if last {
				return bad("The last tier must be open-ended (leave \"to\" empty)")
			}
			if *t.To < t.From {
				return bad("Tier %d ends before it starts", n)
			}
			next = *t.To + 1
		}
		prev = t.Amount
	}
	return nil
}

// TopTierAmount is the highest per-bidder payout (the last tier).
func (p *ReferralProgram) TopTierAmount() Money {
	if len(p.Tiers) == 0 {
		return Zero()
	}
	return p.Tiers[len(p.Tiers)-1].Amount
}

// QualifiesAuction reports whether a paid ticket on auctionID can qualify a referral.
func (p *ReferralProgram) QualifiesAuction(auctionID uuid.UUID) bool {
	if len(p.QualifyingAuctionIDs) == 0 {
		return true
	}
	for _, id := range p.QualifyingAuctionIDs {
		if id == auctionID {
			return true
		}
	}
	return false
}

// ReferralTierProgress is one tier's line in the earnings breakdown.
type ReferralTierProgress struct {
	From     int   `json:"from"`
	To       *int  `json:"to,omitempty"`
	Amount   Money `json:"amount"`
	Bidders  int   `json:"bidders"`  // qualified bidders paid at this tier
	Subtotal Money `json:"subtotal"` // Bidders x Amount
}

// ReferralEarnings is what a number of qualified bidders earns under a program.
type ReferralEarnings struct {
	Qualified int   `json:"qualified"`
	Threshold int   `json:"threshold"`
	Unlocked  bool  `json:"unlocked"`  // threshold reached
	Remaining int   `json:"remaining"` // qualified bidders still needed to unlock (0 once unlocked)
	Earned    Money `json:"earned"`
	// CurrentRate is what the NEXT qualified bidder will pay out (0 before the threshold).
	CurrentRate Money `json:"currentRate"`
	// NextTier is the next higher-paying tier, with how many more bidders reach it (nil at the top).
	NextTier *ReferralNextTier      `json:"nextTier,omitempty"`
	Tiers    []ReferralTierProgress `json:"tiers"`
}

type ReferralNextTier struct {
	From          int   `json:"from"`
	Amount        Money `json:"amount"`
	BiddersNeeded int   `json:"biddersNeeded"`
}

// Earnings computes payouts for `qualified` bidders: nothing below the threshold; from there on
// each bidder pays the rate of the tier its number falls in.
func (p *ReferralProgram) Earnings(qualified int) ReferralEarnings {
	if qualified < 0 {
		qualified = 0
	}
	e := ReferralEarnings{
		Qualified: qualified,
		Threshold: p.Threshold,
		Unlocked:  qualified >= p.Threshold,
		Earned:    Zero(),
		Tiers:     make([]ReferralTierProgress, 0, len(p.Tiers)),
	}
	if !e.Unlocked {
		e.Remaining = p.Threshold - qualified
	}
	nextNumber := qualified + 1
	e.CurrentRate = Zero()
	for _, t := range p.Tiers {
		row := ReferralTierProgress{From: t.From, To: t.To, Amount: t.Amount, Subtotal: Zero()}
		if qualified >= t.From {
			upper := qualified
			if t.To != nil && *t.To < upper {
				upper = *t.To
			}
			row.Bidders = upper - t.From + 1
			row.Subtotal = t.Amount.Mul(decimal.NewFromInt(int64(row.Bidders)))
			e.Earned = e.Earned.Add(row.Subtotal)
		}
		if nextNumber >= t.From && (t.To == nil || nextNumber <= *t.To) {
			e.CurrentRate = t.Amount
		}
		if e.NextTier == nil && t.From > nextNumber && e.CurrentRate.LessThan(t.Amount) {
			e.NextTier = &ReferralNextTier{From: t.From, Amount: t.Amount, BiddersNeeded: t.From - qualified - 1}
		}
		e.Tiers = append(e.Tiers, row)
	}
	return e
}

// ReferralCode is one shareable code. OwnerUserID is the agent's (or influencer's) account;
// it may be empty for an influencer who has no account yet.
type ReferralCode struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	Code        string     `gorm:"type:varchar(20);not null" json:"code"`
	Kind        string     `gorm:"type:varchar(20);not null" json:"kind"`
	OwnerUserID *uuid.UUID `gorm:"type:uuid" json:"ownerUserId,omitempty"`
	Label       string     `gorm:"type:varchar(120);not null;default:''" json:"label"`
	ProgramID   *uuid.UUID `gorm:"type:uuid" json:"programId,omitempty"`
	Status      string     `gorm:"type:varchar(20);not null;default:'active'" json:"status"`
	CreatedBy   *uuid.UUID `gorm:"type:uuid" json:"createdBy,omitempty"`
	// Where the owner wants to be paid (see ReferralPayoutAccount). Empty until they add it.
	PayoutMethod  string    `gorm:"type:varchar(20);not null;default:''" json:"payoutMethod"`
	PayoutBank    string    `gorm:"type:varchar(60);not null;default:''" json:"payoutBank"`
	PayoutAccount string    `gorm:"type:varchar(40);not null;default:''" json:"payoutAccount"`
	PayoutName    string    `gorm:"type:varchar(120);not null;default:''" json:"payoutName"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (ReferralCode) TableName() string { return "referral_codes" }

// SelfSignup reports whether the owner created the code themselves ("Become an agent") rather
// than staff creating it for them.
func (c *ReferralCode) SelfSignup() bool {
	return c.CreatedBy != nil && c.OwnerUserID != nil && *c.CreatedBy == *c.OwnerUserID
}

// ReferralClick is one visitor's visits through a code's link (one row per code and visitor).
type ReferralClick struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CodeID      uuid.UUID `gorm:"type:uuid;not null" json:"codeId"`
	VisitorID   string    `gorm:"type:varchar(64);not null" json:"visitorId"`
	Hits        int       `gorm:"not null;default:1" json:"hits"`
	IPHash      string    `gorm:"type:varchar(64);not null;default:''" json:"-"`
	UserAgent   string    `gorm:"type:varchar(255);not null;default:''" json:"-"`
	LandingPath string    `gorm:"type:varchar(255);not null;default:''" json:"landingPath"`
	FirstSeenAt time.Time `gorm:"not null" json:"firstSeenAt"`
	LastSeenAt  time.Time `gorm:"not null" json:"lastSeenAt"`
}

func (ReferralClick) TableName() string { return "referral_clicks" }

// ReferralAttribution links one signed-up account to the code that brought it. An account is
// attributed at most once (first code wins).
type ReferralAttribution struct {
	ID                  uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CodeID              uuid.UUID  `gorm:"type:uuid;not null" json:"codeId"`
	ReferredUserID      uuid.UUID  `gorm:"type:uuid;not null" json:"referredUserId"`
	VisitorID           string     `gorm:"type:varchar(64);not null;default:''" json:"-"`
	Status              string     `gorm:"type:varchar(20);not null;default:'signed_up'" json:"status"`
	SignedUpAt          time.Time  `gorm:"not null" json:"signedUpAt"`
	QualifiedAt         *time.Time `json:"qualifiedAt,omitempty"`
	QualifyingTxRef     string     `gorm:"type:varchar(255);not null;default:''" json:"-"`
	QualifyingAuctionID *uuid.UUID `gorm:"type:uuid" json:"qualifyingAuctionId,omitempty"`
	QualifiedPhone      string     `gorm:"type:varchar(32)" json:"-"`
	QualifiedAccount    string     `gorm:"type:varchar(255)" json:"-"`
	RejectReason        string     `gorm:"type:varchar(40);not null;default:''" json:"rejectReason,omitempty"`
	RejectNote          string     `gorm:"type:varchar(255);not null;default:''" json:"rejectNote,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

func (ReferralAttribution) TableName() string { return "referral_attributions" }

// ReferralPayout records money staff paid out to a code owner (outside the platform, e.g. a
// telebirr transfer). Earnings minus payouts is what is still owed.
type ReferralPayout struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CodeID    uuid.UUID  `gorm:"type:uuid;not null" json:"codeId"`
	Amount    Money      `gorm:"type:decimal(12,2);not null" json:"amount"`
	Method    string     `gorm:"type:varchar(40);not null;default:''" json:"method"`
	Reference string     `gorm:"type:varchar(120);not null;default:''" json:"reference"`
	Note      string     `gorm:"type:varchar(255);not null;default:''" json:"note"`
	PaidBy    *uuid.UUID `gorm:"type:uuid" json:"paidBy,omitempty"`
	PaidAt    time.Time  `gorm:"not null" json:"paidAt"`
	CreatedAt time.Time  `json:"createdAt"`
}

func (ReferralPayout) TableName() string { return "referral_payouts" }

var referralCodePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]*$`)

// NormalizeReferralCode upper-cases and trims a code as typed or read from a link.
func NormalizeReferralCode(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

// ValidateReferralCode checks a custom code (e.g. an influencer's name): 4-20 letters, digits,
// "-" or "_", starting with a letter or digit.
func ValidateReferralCode(code string) error {
	if len(code) < referralCodeMinLen || len(code) > referralCodeMaxLen || !referralCodePattern.MatchString(code) {
		return NewAppError(400, fmt.Sprintf("Code must be %d-%d letters or digits (\"-\" and \"_\" allowed)", referralCodeMinLen, referralCodeMaxLen), ErrInvalidInput)
	}
	return nil
}

// GenerateReferralCode returns a random 6-character code without look-alike characters.
func GenerateReferralCode() (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(referralGeneratedCodeAlphabet)))
	for i := 0; i < referralGeneratedCodeLength; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(referralGeneratedCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// PhoneDigits reduces a phone number to digits in international form (09.. -> 2519..), which is
// what the one-referral-per-phone rule compares.
func PhoneDigits(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(d, "0") && len(d) == 10:
		d = "251" + d[1:]
	case len(d) == 9 && (strings.HasPrefix(d, "9") || strings.HasPrefix(d, "7")):
		d = "251" + d
	}
	return d
}

// MaskPhone keeps the last 2 digits: "+251 ••• ••• •34".
func MaskPhone(phone string) string {
	d := PhoneDigits(phone)
	if len(d) < 4 {
		return ""
	}
	return "•••• ••" + d[len(d)-2:]
}

// MaskName keeps the first name and the initial of the last: "Abebe K.".
func MaskName(full string) string {
	parts := strings.Fields(full)
	if len(parts) == 0 {
		return ""
	}
	first := parts[0]
	if len(parts) == 1 {
		return first
	}
	last := []rune(parts[len(parts)-1])
	return first + " " + strings.ToUpper(string(last[0])) + "."
}

// SortTiers orders tiers by their starting bidder (input may arrive unordered from a form).
func SortTiers(tiers []ReferralTier) {
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].From < tiers[j].From })
}
