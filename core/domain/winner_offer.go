package domain

import (
	"time"

	"github.com/google/uuid"
)

// Pay only if you win
//
// Tickets cost the entrance fee only. After the draw the lowest unique bid is offered the prize
// and must pay its bid amount within WinnerPaymentWindow. If it does not, the offer expires and
// the prize passes to the next lowest unique bid (by the published draw results, skipping every
// participant who already let an offer lapse). After MaxWinnerAttempts lapsed offers, or when no
// other unique bid is left, the auction is cancelled.
const (
	// WinnerPaymentWindow is how long a winner has to pay their bid amount.
	WinnerPaymentWindow = 48 * time.Hour
	// MaxWinnerAttempts is how many winners in a row may be offered the prize.
	MaxWinnerAttempts = 3
	// WinnerFirstReminderBefore / WinnerSecondReminderBefore: the winner is reminded twice before
	// the deadline. The second reminder also alerts admin to call the winner.
	WinnerFirstReminderBefore  = 24 * time.Hour
	WinnerSecondReminderBefore = 2 * time.Hour
	// WinnerPaymentGrace keeps an offer open past its deadline while a payment the winner started
	// before the deadline is still being confirmed by Chapa.
	WinnerPaymentGrace = 30 * time.Minute
)

// WinnerOffer statuses.
const (
	WinnerOfferAwaitingPayment = "awaiting_payment"
	WinnerOfferPaid            = "paid"
	WinnerOfferExpired         = "expired"
)

// WinnerOffer is one "you won, pay your bid" offer. An auction has at most one awaiting offer
// and at most one paid offer at a time (enforced in the database).
type WinnerOffer struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	AuctionID     uuid.UUID  `gorm:"type:uuid;not null;index:idx_winner_offers_auction" json:"auctionId"`
	BidID         uuid.UUID  `gorm:"type:uuid;not null" json:"bidId"`
	ParticipantID string     `gorm:"type:uuid;not null" json:"participantId"`
	BidValue      Money      `gorm:"type:decimal(10,2);not null" json:"bidValue"`
	Attempt       int        `gorm:"not null" json:"attempt"`
	Status        string     `gorm:"type:varchar(20);not null;default:'awaiting_payment'" json:"status"`
	DueAt         time.Time  `gorm:"not null" json:"dueAt"`
	RemindersSent int        `gorm:"not null;default:0" json:"remindersSent"`
	PaidAt        *time.Time `json:"paidAt,omitempty"`
	PaymentTxRef  string     `gorm:"type:varchar(255)" json:"paymentTxRef,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`

	// Staff-only enrichment (not stored).
	ParticipantName  string `gorm:"-" json:"participantName,omitempty"`
	ParticipantPhone string `gorm:"-" json:"participantPhone,omitempty"`
	ParticipantEmail string `gorm:"-" json:"participantEmail,omitempty"`
}

func (WinnerOffer) TableName() string { return "auction_winner_offers" }

// IsAwaiting reports whether the offer still waits for the winner's payment.
func (o *WinnerOffer) IsAwaiting() bool {
	return o != nil && o.Status == WinnerOfferAwaitingPayment
}

// CanPayAt reports whether the winner may still start a payment at time now.
func (o *WinnerOffer) CanPayAt(now time.Time) bool {
	return o.IsAwaiting() && now.Before(o.DueAt)
}

// ReminderDue returns which reminder (1 or 2) should go out at time now, or 0 for none.
// Reminders are sent in order, so a late scheduler sends the first one before the second.
func (o *WinnerOffer) ReminderDue(now time.Time) int {
	if !o.IsAwaiting() || !now.Before(o.DueAt) {
		return 0
	}
	left := o.DueAt.Sub(now)
	switch {
	case o.RemindersSent == 0 && left <= WinnerFirstReminderBefore:
		return 1
	case o.RemindersSent == 1 && left <= WinnerSecondReminderBefore:
		return 2
	}
	return 0
}

// WinnerPaymentStatus is the public, per-auction summary of the winner's payment.
const (
	WinnerPaymentStatusNone     = ""                 // no draw yet / no winner
	WinnerPaymentStatusAwaiting = "awaiting_payment" // a winner was drawn and has not paid yet
	WinnerPaymentStatusPaid     = "paid"             // the winner paid; the sale is complete
	WinnerPaymentStatusUnpaid   = "unpaid"           // every offered winner let the deadline pass
)

// MyAuctionResult is what the signed-in participant sees about their own outcome.
type MyAuctionResult struct {
	// Outcome: "pending" (no draw yet), "won_pay_now", "won_paid", "won_expired", "lost", "no_winner", "cancelled".
	Outcome  string     `json:"outcome"`
	BidValue *Money     `json:"bidValue,omitempty"`
	DueAt    *time.Time `json:"dueAt,omitempty"`
	Attempt  int        `json:"attempt,omitempty"`
}

const (
	MyOutcomePending    = "pending"
	MyOutcomeWonPayNow  = "won_pay_now"
	MyOutcomeWonPaid    = "won_paid"
	MyOutcomeWonExpired = "won_expired"
	MyOutcomeLost       = "lost"
	MyOutcomeNoWinner   = "no_winner"
	MyOutcomeCancelled  = "cancelled"
)

// Payment purposes.
const (
	PaymentPurposeEntry  = "entry"  // ticket checkout (entrance fees)
	PaymentPurposeWinner = "winner" // the winner paying their bid amount
)
