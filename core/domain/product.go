package domain

import (
	"time"

	"github.com/google/uuid"
)

// AuctionStatus represents the lifecycle state of a Lowest Unique Bid auction.
type AuctionStatus string

const (
	AuctionStatusDraft     AuctionStatus = "draft"
	AuctionStatusActive    AuctionStatus = "active"
	AuctionStatusPostponed AuctionStatus = "postponed" // CloseAt extended by admin
	AuctionStatusClosed    AuctionStatus = "closed"    // Bidding over, resolving winner
	AuctionStatusExecuted  AuctionStatus = "executed"  // Winner determined
	AuctionStatusCancelled AuctionStatus = "cancelled"
)

// Auction is a Lowest Unique Bid (LUB) auction item.
// Participants pay an entrance fee to unlock bidding.
// The winner is the participant whose bid_value is the lowest value
// placed by exactly one person.
type Auction struct {
	ID          uuid.UUID     `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	Title       string        `gorm:"type:varchar(255);not null"                     json:"title"`
	Description string        `gorm:"type:text"                                      json:"description"`
	ImageURL    string        `gorm:"type:text"                                      json:"imageUrl,omitempty"`
	ImageURLs   []string      `gorm:"type:jsonb;serializer:json"                      json:"imageUrls,omitempty"`
	VideoURL    string        `gorm:"type:text"                                      json:"videoUrl,omitempty"`
	Status      AuctionStatus `gorm:"type:auction_status;not null;default:'draft';index:idx_auctions_status;index:idx_auctions_due_close,priority:1" json:"status"`

	// Financials & Rules
	EntranceFee     Money  `gorm:"type:decimal(12,2);not null"              json:"entranceFee"`
	RetailValue     Money  `gorm:"type:decimal(12,2);not null;default:0"   json:"retailValue"`
	MinParticipants int    `gorm:"not null;default:10"                     json:"minParticipants"`
	MinBidValue     Money  `gorm:"type:decimal(10,2);not null;default:0.01" json:"minBidValue"`
	MaxBidValue     *Money `gorm:"type:decimal(10,2)"                       json:"maxBidValue,omitempty"`

	// Free entrance promo (admin-controlled per auction): the first N joiners skip the
	// entrance fee but still pay their bid amount via Chapa. Limit 0 means "all".
	// Each customer may use the promo on one auction only (enforced in DB).
	// No gorm defaults on purpose: false / 0 are meaningful and must be written as-is.
	FreeEntranceEnabled bool `gorm:"not null" json:"freeEntranceEnabled"`
	FreeEntranceLimit   int  `gorm:"not null" json:"freeEntranceLimit"`
	// FreeEntranceClaimed counts reserved/used free slots. Only the payment ledger changes it.
	FreeEntranceClaimed int `gorm:"not null" json:"freeEntranceClaimed"`

	// Schedule
	StartAt    *time.Time `gorm:"column:start_at"                                                          json:"startAt,omitempty"`
	CloseAt    *time.Time `gorm:"column:close_at;index:idx_auctions_due_close,priority:2"                  json:"closeAt,omitempty"`
	ExecutedAt *time.Time `gorm:"column:executed_at"                                                       json:"executedAt,omitempty"`

	// Winner Reference (set after ExecuteDraw)
	WinnerBidID         *uuid.UUID `gorm:"type:uuid;index:idx_auctions_winner_bid"         json:"winnerBidId,omitempty"`
	WinnerParticipantID *string    `gorm:"type:uuid;index:idx_auctions_winner_participant" json:"winnerParticipantId,omitempty"`
	WinnerBidValue      *Money     `gorm:"type:decimal(10,2)"                              json:"winnerBidValue,omitempty"`
	WinnerName          string     `gorm:"-"                                               json:"winnerName,omitempty"`
	TotalBids           int64      `gorm:"-"                                               json:"totalBids"`
	ParticipantCount    int64      `gorm:"-"                                               json:"participantCount"`
	UserHasJoined       bool       `gorm:"-"                                               json:"userHasJoined"`
	CardTag             string     `gorm:"-"                                               json:"tag,omitempty"` // hot | new | almost_unlocked
	FreeEntranceRemaining       int  `gorm:"-" json:"freeEntranceRemaining"`
	FreeEntranceUnlimited       bool `gorm:"-" json:"freeEntranceUnlimited"`
	UserEligibleForFreeEntrance bool `gorm:"-" json:"userEligibleForFreeEntrance"`

	CreatedBy uuid.UUID `gorm:"type:uuid;not null;index:idx_auctions_created_by" json:"createdBy"`
	CreatedAt time.Time `gorm:"index:idx_auctions_created_at"                    json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Relations
	Creator *User `gorm:"foreignKey:CreatedBy;references:ID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"-"`
}

// PublicAuctionCard is the list/card payload: no live bid values.
type PublicAuctionCard struct {
	ID               uuid.UUID     `json:"id"`
	Title            string        `json:"title"`
	Description      string        `json:"description,omitempty"`
	ImageURL         string        `json:"imageUrl,omitempty"`
	ImageURLs        []string      `json:"imageUrls,omitempty"`
	VideoURL         string        `json:"videoUrl,omitempty"`
	Status           AuctionStatus `json:"status"`
	EntranceFee      Money         `json:"entranceFee"`
	RetailValue      Money         `json:"retailValue"`
	MinParticipants  int           `json:"minParticipants"`
	ParticipantCount int64         `json:"participantCount"`
	CloseAt          *time.Time    `json:"closeAt,omitempty"`
	EndsAt           *time.Time    `json:"endsAt,omitempty"`
	CreatedAt        time.Time     `json:"createdAt"`
	UserHasJoined    bool          `json:"userHasJoined"`
	Tag              string        `json:"tag,omitempty"`
	WinnerName       string        `json:"winnerName,omitempty"`
	PrizeUnlocked    bool          `json:"prizeUnlocked"`
	FreeEntranceEnabled         bool `json:"freeEntranceEnabled"`
	FreeEntranceLimit           int  `json:"freeEntranceLimit"`
	FreeEntranceRemaining       int  `json:"freeEntranceRemaining"`
	FreeEntranceUnlimited       bool `json:"freeEntranceUnlimited"`
	UserEligibleForFreeEntrance bool `json:"userEligibleForFreeEntrance"`
}

func (Auction) TableName() string { return "auctions" }

// AuctionAuditEntry represents one distinct bid value in an executed auction's public audit log.
type AuctionAuditEntry struct {
	BidValue Money `json:"bidValue"`
	Count    int   `json:"count"`
	IsUnique bool  `json:"isUnique"`
	IsWinner bool  `json:"isWinner"`
}

// AuctionAudit represents the published public audit results of a completed auction.
type AuctionAudit struct {
	AuctionID           uuid.UUID           `json:"auctionId"`
	Title               string              `json:"title"`
	Status              AuctionStatus       `json:"status"`
	WinnerBidValue      *Money              `json:"winnerBidValue,omitempty"`
	WinnerName          string              `json:"winnerName,omitempty"`
	TotalBids           int64               `json:"totalBids"`
	UniqueBidsCount     int                 `json:"uniqueBidsCount"`
	DuplicatedBidsCount int                 `json:"duplicatedBidsCount"`
	MinBidValue         Money               `json:"minBidValue"`
	MaxBidValue         *Money              `json:"maxBidValue,omitempty"`
	Entries             []AuctionAuditEntry `json:"entries"`
}
