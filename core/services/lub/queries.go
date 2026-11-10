package lub

import (
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

const (
	cardTagHot            = "hot"
	cardTagNew            = "new"
	cardTagAlmostUnlocked = "almost_unlocked"
	defaultMinParticipants = 10
	almostUnlockedRatio   = 0.8
	newListingWindow      = 48 * time.Hour
	hotGrowthWindow       = 24 * time.Hour
)

// ---------------------- Auction queries ----------------------

func (s *Service) GetAuctionByID(id uuid.UUID) (*domain.Auction, error) {
	return s.auctionRepo.GetByID(id)
}

func (s *Service) ListAuctions(status, search string, page, limit int) ([]domain.Auction, int64, error) {
	offset := (page - 1) * limit
	return s.auctionRepo.List(status, search, limit, offset)
}

func (s *Service) ListPublicAuctions(page, limit int) ([]domain.Auction, int64, error) {
	offset := (page - 1) * limit
	return s.auctionRepo.ListPublic(limit, offset)
}

// ListPublicAuctionCards returns list payloads without bid values.
func (s *Service) ListPublicAuctionCards(page, limit int, viewerID string) ([]domain.PublicAuctionCard, int64, error) {
	cards, total, _, err := s.ListPublicAuctionCardsFiltered(ports.PublicAuctionFilter{
		Limit:  limit,
		Offset: (page - 1) * limit,
	}, viewerID)
	return cards, total, err
}

// ListPublicAuctionCardsFiltered returns server-filtered, sorted, and paginated auction cards with status counts.
func (s *Service) ListPublicAuctionCardsFiltered(filter ports.PublicAuctionFilter, viewerID string) ([]domain.PublicAuctionCard, int64, map[string]int64, error) {
	auctions, total, statusCounts, err := s.auctionRepo.ListPublicFiltered(filter)
	if err != nil {
		return nil, 0, nil, err
	}
	return s.toPublicAuctionCards(auctions, viewerID), total, statusCounts, nil
}

func (s *Service) toPublicAuctionCards(auctions []domain.Auction, viewerID string) []domain.PublicAuctionCard {
	if len(auctions) == 0 {
		return []domain.PublicAuctionCard{}
	}
	now := time.Now().UTC()
	sinceHot := now.Add(-hotGrowthWindow)

	auctionIDs := make([]uuid.UUID, 0, len(auctions))
	for i := range auctions {
		auctionIDs = append(auctionIDs, auctions[i].ID)
	}

	countsMap, _ := s.participationRepo.CountByAuctions(auctionIDs)
	growthMap, _ := s.participationRepo.CountByAuctionsSince(auctionIDs, sinceHot)
	var joinedMap map[uuid.UUID]bool
	if viewerID != "" {
		joinedMap, _ = s.participationRepo.JoinedAuctionSet(auctionIDs, viewerID)
	}
	viewerUsedFree := false
	if viewerID != "" {
		var err error
		if viewerUsedFree, err = s.participationRepo.HasUsedFreeEntrance(viewerID); err != nil {
			viewerUsedFree = true // unknown -> never promise a free entrance we can't confirm
		}
	}

	var maxGrowth int64
	for i := range auctions {
		a := &auctions[i]
		a.ParticipantCount = countsMap[a.ID]
		g := growthMap[a.ID]
		if g > maxGrowth {
			maxGrowth = g
		}
		if viewerID != "" {
			a.UserHasJoined = joinedMap[a.ID]
		}
		a.FreeEntranceRemaining = a.FreeSlotsRemaining()
		a.FreeEntranceUnlimited = a.FreeEntranceIsUnlimited()
		a.UserEligibleForFreeEntrance = a.FreeEntranceAvailable() && viewerID != "" && !viewerUsedFree && !a.UserHasJoined
	}

	cards := make([]domain.PublicAuctionCard, 0, len(auctions))
	for i := range auctions {
		cards = append(cards, toPublicAuctionCard(auctions[i], growthMap[auctions[i].ID], maxGrowth, now))
	}
	return cards
}

func toPublicAuctionCard(a domain.Auction, growth24h, maxGrowth int64, now time.Time) domain.PublicAuctionCard {
	minP := a.MinParticipants
	if minP <= 0 {
		minP = defaultMinParticipants
	}
	unlocked := a.ParticipantCount >= int64(minP)
	tag := computeCardTag(a, growth24h, maxGrowth, minP, unlocked, now)

	endsAt := a.CloseAt
	winnerName := ""
	if a.Status == domain.AuctionStatusExecuted {
		winnerName = a.WinnerName
	}

	return domain.PublicAuctionCard{
		ID:                          a.ID,
		Title:                       a.Title,
		Description:                 a.Description,
		ImageURL:                    a.ImageURL,
		ImageURLs:                   a.ImageURLs,
		VideoURL:                    a.VideoURL,
		Status:                      a.Status,
		EntranceFee:                 a.EntranceFee,
		RetailValue:                 a.RetailValue,
		MinParticipants:             minP,
		ParticipantCount:            a.ParticipantCount,
		CloseAt:                     a.CloseAt,
		EndsAt:                      endsAt,
		CreatedAt:                   a.CreatedAt,
		UserHasJoined:               a.UserHasJoined,
		Tag:                         tag,
		WinnerName:                  winnerName,
		PrizeUnlocked:               unlocked,
		FreeEntranceEnabled:         a.FreeEntranceEnabled,
		FreeEntranceLimit:           a.FreeEntranceLimit,
		FreeEntranceRemaining:       a.FreeEntranceRemaining,
		FreeEntranceUnlimited:       a.FreeEntranceUnlimited,
		UserEligibleForFreeEntrance: a.UserEligibleForFreeEntrance,
	}
}

func computeCardTag(a domain.Auction, growth24h, maxGrowth int64, minP int, unlocked bool, now time.Time) string {
	st := a.Status
	if st != domain.AuctionStatusActive && st != domain.AuctionStatusPostponed {
		return ""
	}
	// Priority: hot > almost_unlocked > new
	if maxGrowth > 0 && growth24h == maxGrowth {
		return cardTagHot
	}
	if !unlocked && minP > 0 {
		ratio := float64(a.ParticipantCount) / float64(minP)
		if ratio >= almostUnlockedRatio {
			return cardTagAlmostUnlocked
		}
	}
	if now.Sub(a.CreatedAt) <= newListingWindow {
		return cardTagNew
	}
	return ""
}

// ---------------------- Participation queries ----------------------

func (s *Service) GetParticipation(auctionID uuid.UUID, participantID string) (*domain.AuctionParticipation, error) {
	return s.participationRepo.GetByAuctionAndParticipant(auctionID, participantID)
}

func (s *Service) ListParticipations(auctionID uuid.UUID, page, limit int) ([]domain.AuctionParticipation, int64, error) {
	offset := (page - 1) * limit
	return s.participationRepo.ListByAuction(auctionID, limit, offset)
}

func (s *Service) ListParticipantParticipations(participantID string, page, limit int) ([]domain.AuctionParticipation, int64, error) {
	offset := (page - 1) * limit
	return s.participationRepo.ListByParticipant(participantID, limit, offset)
}

func (s *Service) CountParticipants(auctionID uuid.UUID) (int64, error) {
	return s.participationRepo.CountByAuction(auctionID)
}

// ---------------------- Bid queries ----------------------

func (s *Service) GetBid(id uuid.UUID) (*domain.Bid, error) {
	b, err := s.bidRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	b.PopulateUpdateState()
	return b, nil
}

func (s *Service) ListMyBids(auctionID uuid.UUID, participantID string) ([]domain.Bid, error) {
	bids, err := s.bidRepo.ListByAuctionAndParticipant(auctionID, participantID)
	if err != nil {
		return nil, err
	}
	for i := range bids {
		bids[i].PopulateUpdateState()
	}
	return bids, nil
}

func (s *Service) ListAuctionBids(auctionID uuid.UUID, page, limit int) ([]domain.Bid, int64, error) {
	offset := (page - 1) * limit
	bids, total, err := s.bidRepo.ListByAuction(auctionID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	for i := range bids {
		bids[i].PopulateUpdateState()
	}
	return bids, total, nil
}

func (s *Service) GetAuctionAudit(auctionID uuid.UUID) (*domain.AuctionAudit, error) {
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, domain.NewAppError(404, "Auction not found", nil)
	}

	if a.Status != domain.AuctionStatusExecuted && a.Status != domain.AuctionStatusClosed {
		return nil, domain.NewAppError(403, "Audit log is published only after the auction round has completed", nil)
	}

	bids, total, err := s.bidRepo.ListByAuction(auctionID, 10000, 0)
	if err != nil {
		return nil, err
	}

	type valGroup struct {
		bidValue domain.Money
		count    int
		isWinner bool
	}
	var groups []valGroup
	groupMap := make(map[string]int)

	for _, b := range bids {
		valStr := b.BidValue.String()
		idx, exists := groupMap[valStr]
		if !exists {
			groupMap[valStr] = len(groups)
			groups = append(groups, valGroup{
				bidValue: b.BidValue,
				count:    1,
				isWinner: b.IsWinner,
			})
		} else {
			groups[idx].count++
			if b.IsWinner {
				groups[idx].isWinner = true
			}
		}
	}

	uniqueCount := 0
	duplicateCount := 0
	entries := make([]domain.AuctionAuditEntry, len(groups))

	for i, g := range groups {
		isUnique := g.count == 1
		if isUnique {
			uniqueCount++
		} else {
			duplicateCount += g.count
		}
		isWinner := g.isWinner
		if a.WinnerBidValue != nil && g.bidValue.Equal(*a.WinnerBidValue) {
			isWinner = true
		}
		entries[i] = domain.AuctionAuditEntry{
			BidValue: g.bidValue,
			Count:    g.count,
			IsUnique: isUnique,
			IsWinner: isWinner,
		}
	}

	audit := &domain.AuctionAudit{
		AuctionID:           a.ID,
		Title:               a.Title,
		Status:              a.Status,
		WinnerBidValue:      a.WinnerBidValue,
		WinnerName:          a.WinnerName,
		TotalBids:           total,
		UniqueBidsCount:     uniqueCount,
		DuplicatedBidsCount: duplicateCount,
		MinBidValue:         a.MinBidValue,
		MaxBidValue:         a.MaxBidValue,
		Entries:             entries,
	}

	return audit, nil
}
