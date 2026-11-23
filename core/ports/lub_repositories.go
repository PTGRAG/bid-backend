package ports

import (
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// PublicAuctionFilter defines server-side filtering, sorting, and pagination for public auctions.
type PublicAuctionFilter struct {
	Status string
	Search string
	Sort   string
	Limit  int
	Offset int
}

// AuctionRepository manages Lowest Unique Bid auctions.
type AuctionRepository interface {
	Create(a *domain.Auction) error
	GetByID(id uuid.UUID) (*domain.Auction, error)
	Update(a *domain.Auction) error
	UpdateStatus(id uuid.UUID, status domain.AuctionStatus) error
	List(status string, search string, limit, offset int) ([]domain.Auction, int64, error)
	ListPublic(limit, offset int) ([]domain.Auction, int64, error)
	ListPublicFiltered(filter PublicAuctionFilter) ([]domain.Auction, int64, map[string]int64, error)
	// ListDueForClose returns active auctions whose close_at has passed (for the scheduler).
	ListDueForClose() ([]domain.Auction, error)
	Delete(id uuid.UUID) error
}

// AuctionParticipationRepository manages entrance-fee payment records.
type AuctionParticipationRepository interface {
	Create(p *domain.AuctionParticipation) error
	GetByAuctionAndParticipant(auctionID uuid.UUID, participantID string) (*domain.AuctionParticipation, error)
	GetByTransactionID(txID string) (*domain.AuctionParticipation, error)
	ListByAuction(auctionID uuid.UUID, limit, offset int) ([]domain.AuctionParticipation, int64, error)
	ListByParticipant(participantID string, limit, offset int) ([]domain.AuctionParticipation, int64, error)
	UpdateStatus(id uuid.UUID, status string) error
	CountByAuction(auctionID uuid.UUID) (int64, error)
	CountByAuctionSince(auctionID uuid.UUID, since time.Time) (int64, error)
	CountByAuctions(auctionIDs []uuid.UUID) (map[uuid.UUID]int64, error)
	CountByAuctionsSince(auctionIDs []uuid.UUID, since time.Time) (map[uuid.UUID]int64, error)
	JoinedAuctionSet(auctionIDs []uuid.UUID, participantID string) (map[uuid.UUID]bool, error)
	HasUsedFreeEntrance(participantID string) (bool, error)
	GetCountByMonth(lastMonths int) ([]domain.MonthlyCount, error)
}

// BidRepository manages the secret bid values participants place.
type BidRepository interface {
	Create(b *domain.Bid) error
	GetByID(id uuid.UUID) (*domain.Bid, error)
	Update(b *domain.Bid) error
	GetByAuctionAndParticipantAndValue(auctionID uuid.UUID, participantID string, bidValue domain.Money) (*domain.Bid, error)
	ListByAuctionAndParticipant(auctionID uuid.UUID, participantID string) ([]domain.Bid, error)
	ListByAuction(auctionID uuid.UUID, limit, offset int) ([]domain.Bid, int64, error)
	FindWinner(auctionID uuid.UUID) (*domain.Bid, error)
	// FindWinnerExcluding returns the lowest unique bid (uniqueness counted over ALL bids, as
	// published in the draw results) whose participant is not in excludeParticipantIDs.
	FindWinnerExcluding(auctionID uuid.UUID, excludeParticipantIDs []string) (*domain.Bid, error)
	MarkWinner(auctionID uuid.UUID, winnerBidID uuid.UUID) error
	// MoveWinner clears is_winner on the auction's bids and sets it on newWinnerBidID.
	MoveWinner(auctionID uuid.UUID, newWinnerBidID uuid.UUID) error
	DeleteBid(id uuid.UUID) error
	GetCountByMonth(lastMonths int) ([]domain.MonthlyCount, error)
}

// WinnerOfferRepository stores pay-only-if-you-win offers (see domain.WinnerOffer).
type WinnerOfferRepository interface {
	Create(o *domain.WinnerOffer) error
	GetByID(id uuid.UUID) (*domain.WinnerOffer, error)
	// GetOpen returns the auction's awaiting offer, or nil.
	GetOpen(auctionID uuid.UUID) (*domain.WinnerOffer, error)
	// GetLatest returns the auction's highest-attempt offer, or nil.
	GetLatest(auctionID uuid.UUID) (*domain.WinnerOffer, error)
	ListByAuction(auctionID uuid.UUID) ([]domain.WinnerOffer, error)
	// LatestByAuctions returns the highest-attempt offer per auction (missing = no offer).
	LatestByAuctions(auctionIDs []uuid.UUID) (map[uuid.UUID]domain.WinnerOffer, error)
	// ListOpen returns every awaiting offer (the scheduler checks deadlines and reminders).
	ListOpen(limit int) ([]domain.WinnerOffer, error)
	// MarkPaid flips an awaiting offer to paid. ok=false when it was no longer awaiting.
	MarkPaid(id uuid.UUID, txRef string, paidAt time.Time) (ok bool, err error)
	// MarkExpired flips an awaiting offer to expired. ok=false when it was no longer awaiting.
	MarkExpired(id uuid.UUID) (ok bool, err error)
	// AdvanceReminder moves reminders_sent from `from` to from+1 on an awaiting offer.
	// ok=false means another worker already sent it (or the offer closed).
	AdvanceReminder(id uuid.UUID, from int) (ok bool, err error)
}
