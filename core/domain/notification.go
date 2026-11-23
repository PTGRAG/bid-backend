package domain

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Notification represents an in-app notification
type Notification struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"                                    json:"id"`
	UserID      uuid.UUID  `gorm:"type:uuid;not null;index:idx_notifications_user_unread,priority:1"                 json:"userId"`
	UserType    string     `gorm:"type:varchar(20);not null;index:idx_notifications_user_unread,priority:2"          json:"userType"` // participant, admin
	Type        string     `gorm:"type:varchar(50);not null"                                                         json:"type"`
	Title       string     `gorm:"type:varchar(255);not null"                                                        json:"title"`
	Message     string     `gorm:"type:text;not null"                                                                json:"message"`
	ActionURL   string     `gorm:"type:text"                                                                         json:"actionUrl,omitempty"`
	ActionLabel string     `gorm:"type:varchar(50)"                                                                  json:"actionLabel,omitempty"`
	AuctionID   *uuid.UUID `gorm:"type:uuid;index:idx_notifications_auction"                                         json:"auctionId,omitempty"`
	IsRead      bool       `gorm:"not null;default:false;index:idx_notifications_user_unread,priority:3"             json:"isRead"`
	ReadAt      *time.Time `json:"readAt,omitempty"`
	Priority    string     `gorm:"type:varchar(20);default:'normal'"                                                 json:"priority"` // low, normal, high, urgent
	Metadata    string     `gorm:"type:jsonb"                                                                        json:"metadata,omitempty"`
	CreatedAt   time.Time  `gorm:"autoCreateTime;index:idx_notifications_created_at"                                 json:"createdAt"`
	ExpiresAt   *time.Time `gorm:"index:idx_notifications_expires_at"                                                json:"expiresAt,omitempty"`
	DeletedAt   gorm.DeletedAt `gorm:"index"                                                                         json:"-"`

	// Relations
	Auction *Auction `gorm:"foreignKey:AuctionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"-"`
}

// Notification types
const (
	NotificationTypeParticipationConfirmed = "participation_confirmed"
	NotificationTypeBidPlaced              = "bid_placed"
	NotificationTypeBidUpdated             = "bid_updated"
	NotificationTypeAuctionWinner          = "auction_winner"
	NotificationTypeAuctionNoWinner        = "auction_no_winner"
	NotificationTypeAuctionResult          = "auction_result"          // draw done, you did not win
	NotificationTypeWinnerPaymentReminder  = "winner_payment_reminder" // you won, deadline approaching
	NotificationTypeWinnerPaymentExpired   = "winner_payment_expired"  // you won but did not pay in time
	NotificationTypeWinnerPaid             = "winner_paid"             // your winning payment went through
	NotificationTypeAuctionActivated       = "auction_activated"
	NotificationTypeAuctionClosed          = "auction_closed"
	NotificationTypeAuctionPostponed       = "auction_postponed"
	NotificationTypeAuctionCancelled       = "auction_cancelled"
	NotificationTypePaymentSuccess         = "payment_success"
	NotificationTypePaymentFailed          = "payment_failed"
	NotificationTypeRefundCompleted        = "refund_completed"
	NotificationTypeLegalDocumentUpdated   = "legal_document_updated"
)

// User types
const (
	UserTypeParticipant = "participant"
	UserTypeAgent       = "agent"
	UserTypeAdmin       = "admin"
)

// Notification priorities
const (
	NotificationPriorityLow    = "low"
	NotificationPriorityNormal = "normal"
	NotificationPriorityHigh   = "high"
	NotificationPriorityUrgent = "urgent"
)
