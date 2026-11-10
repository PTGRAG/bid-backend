package domain

import (
	"time"

	"github.com/google/uuid"
)

// AuctionParticipation records that a participant has paid the entrance fee
// for a specific auction, unlocking their ability to place bids.
// Uniqueness rule: (auction_id, participant_id) is unique.
type AuctionParticipation struct {
	ID            uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"                                                                                          json:"id"`
	AuctionID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_participation_auction_participant,priority:1;index:idx_participation_auction_status,priority:1"        json:"auctionId"`
	ParticipantID string    `gorm:"type:uuid;not null;uniqueIndex:ux_participation_auction_participant,priority:2;index:idx_participation_participant"                      json:"participantId"`
	TransactionID string    `gorm:"type:varchar(255);not null;uniqueIndex:ux_participation_tx_id"                                                                           json:"transactionId"`
	FeePaid       Money     `gorm:"type:decimal(12,2);not null"                                                                                                             json:"feePaid"`
	EntranceFree  bool      `gorm:"not null;default:false;index:idx_participation_entrance_free"                                                                            json:"entranceFree"`
	Status        string    `gorm:"type:participation_status;not null;default:'active';index:idx_participation_auction_status,priority:2"                                   json:"status"` // active | refunded
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`

	// Relations
	Auction *Auction `gorm:"foreignKey:AuctionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

func (AuctionParticipation) TableName() string { return "auction_participations" }

const (
	ParticipationStatusActive   = "active"
	ParticipationStatusRefunded = "refunded"
)
