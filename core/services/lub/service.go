package lub

import (
	"context"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

// Service is the Lowest Unique Bid (LUB) auction business-logic layer.
type Service struct {
	auctionRepo       ports.AuctionRepository
	participationRepo ports.AuctionParticipationRepository
	bidRepo           ports.BidRepository
	audit             ports.AuditLogService
	notificationSvc   LUBNotificationProvider
	telegramPoster    TelegramChannelPoster

	// Pay only if you win (optional; see winner.go).
	winnerOffers   ports.WinnerOfferRepository
	admin          ports.AdminNotifier
	paymentChecker WinnerPaymentChecker
	contacts       ParticipantContactLookup
	now            func() time.Time // tests override the clock
}

// LUBNotificationProvider is the subset of notification methods LUB triggers.
type LUBNotificationProvider interface {
	NotifyAuctionActivated(ctx context.Context, productID uuid.UUID, productName string) error
	NotifyAuctionPostponed(ctx context.Context, productID uuid.UUID, productName string, newCloseAt string) error
	NotifyAuctionClosed(ctx context.Context, productID uuid.UUID, productName string) error
	NotifyAuctionCancelled(ctx context.Context, productID uuid.UUID, productName string, reason string) error
	NotifyBidPlaced(ctx context.Context, participantID string, productID uuid.UUID, productName string, bidValue domain.Money) error
	NotifyBidUpdated(ctx context.Context, participantID string, productID uuid.UUID, productName string, oldValue, newValue domain.Money) error
	NotifyAuctionWinner(ctx context.Context, winnerID string, productID uuid.UUID, productName string, winningBid domain.Money) error
	NotifyAuctionNoWinner(ctx context.Context, productID uuid.UUID, productName string) error
}

// TelegramChannelPoster posts newly published auctions to a Telegram channel.
type TelegramChannelPoster interface {
	PostAuctionPublished(ctx context.Context, a *domain.Auction) error
}

func NewService(
	auctionRepo ports.AuctionRepository,
	participationRepo ports.AuctionParticipationRepository,
	bidRepo ports.BidRepository,
	audit ports.AuditLogService,
	notificationSvc LUBNotificationProvider,
	telegramPoster TelegramChannelPoster,
) *Service {
	return &Service{
		auctionRepo:       auctionRepo,
		participationRepo: participationRepo,
		bidRepo:           bidRepo,
		audit:             audit,
		notificationSvc:   notificationSvc,
		telegramPoster:    telegramPoster,
	}
}

func (s *Service) notify(fn func() error) {
	if s.notificationSvc == nil || fn == nil {
		return
	}
	_ = fn()
}

func (s *Service) postTelegram(fn func() error) {
	if s.telegramPoster == nil || fn == nil {
		return
	}
	_ = fn()
}
