package domain

import (
	"time"

	"github.com/google/uuid"
)

func (Payment) TableName() string {
	return "payments"
}

type Payment struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"                                          json:"id"`
	TxRef           string     `gorm:"type:varchar(255);not null;uniqueIndex:ux_payments_tx_ref"                               json:"txRef"`
	ChapaReference  string     `gorm:"type:varchar(255);index:idx_payments_chapa_ref"                                          json:"chapaReference,omitempty"`
	UserID          *uuid.UUID `gorm:"type:uuid;index:idx_payments_user"                                                       json:"userId,omitempty"`
	AuctionID       *uuid.UUID `gorm:"type:uuid;index:idx_payments_auction"                                                    json:"auctionId,omitempty"`
	ParticipationID *uuid.UUID `gorm:"type:uuid;index:idx_payments_participation"                                              json:"participationId,omitempty"`
	Amount          Money      `gorm:"type:decimal(12,2);not null"                                                             json:"amount"`
	// BidValues persists the bid pack (underscore-separated) so Chapa webhooks / other
	// replicas can place bids without relying on in-memory pendingBids.
	BidValues       string     `gorm:"type:text;column:bid_values"                                                             json:"bidValues,omitempty"`
	// EntranceWaived marks a checkout that reserved a free-entrance slot: the customer pays
	// the bid amount(s) only. Set once at checkout from server-side eligibility (never from
	// client or Chapa meta). While pending/success it is unique per user (partial index).
	EntranceWaived  bool       `gorm:"not null;default:false"                                                                  json:"entranceWaived"`
	Status          string     `gorm:"type:payment_status;not null;default:'pending';index:idx_payments_unprocessed,priority:1" json:"status"`
	Processed       bool       `gorm:"not null;default:false;index:idx_payments_unprocessed,priority:2"                        json:"processed"`
	RetryDisabled   bool       `gorm:"not null;default:false;index:idx_payments_unprocessed,priority:3"                        json:"retryDisabled"`
	LastError       string     `gorm:"type:text"                                                                               json:"lastError,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `gorm:"index:idx_payments_updated_at"                                                           json:"updatedAt"`

	// Relations
	Auction       *Auction              `gorm:"foreignKey:AuctionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"       json:"-"`
	Participation *AuctionParticipation `gorm:"foreignKey:ParticipationID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"-"`
}

const (
	PaymentStatusPending  = "pending"
	PaymentStatusSuccess  = "success"
	PaymentStatusFailed   = "failed"
	PaymentStatusRefunded = "refunded"
)

type PaymentStaffView struct {
	Payment
	ParticipantName  string `json:"participantName,omitempty"`
	ParticipantEmail string `json:"participantEmail,omitempty"`
	AuctionTitle     string `json:"auctionTitle,omitempty"`
	HasParticipation bool   `json:"hasParticipation"`
	RefundStatus     string `json:"refundStatus,omitempty"`
}

type MonthlyCount struct {
	Month string `json:"month"`
	Count int64  `json:"count"`
}

type MonthlyAmount struct {
	Month  string `json:"month"`
	Amount Money  `json:"amount"`
}

const (
	RefundStatusPending    = "pending"
	RefundStatusProcessing = "processing"
	RefundStatusSuccess    = "success"
	RefundStatusFailed     = "failed"
	RefundStatusReversed   = "reversed"
)

type RefundRecord struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	AuctionID       uuid.UUID  `gorm:"type:uuid;not null;index:idx_refunds_auction"   json:"auctionId"`
	ParticipationID *uuid.UUID `gorm:"type:uuid;index:idx_refunds_participation"      json:"participationId,omitempty"`
	ParticipantID   string     `gorm:"type:uuid;not null;index:idx_refunds_participant" json:"participantId"`
	Amount          Money      `gorm:"type:decimal(12,2);not null"                    json:"amount"`
	TxRef           string     `gorm:"type:varchar(255);not null;index:idx_refunds_tx_ref" json:"txRef"`
	ChapaRefundRef  string     `gorm:"type:varchar(255)"                              json:"chapaRefundRef,omitempty"`
	Status          string     `gorm:"type:refund_status;not null;default:'pending';index:idx_refunds_status" json:"status"`
	ErrorMessage    string     `gorm:"type:text"                                      json:"errorMessage,omitempty"`
	ProcessedBy     *uuid.UUID `gorm:"type:uuid;index:idx_refunds_processed_by"       json:"processedBy,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`

	// Relations
	Auction       *Auction              `gorm:"foreignKey:AuctionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"       json:"-"`
	Participation *AuctionParticipation `gorm:"foreignKey:ParticipationID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"-"`
	Processor     *User                 `gorm:"foreignKey:ProcessedBy;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"     json:"-"`
}

func (RefundRecord) TableName() string {
	return "refund_records"
}
