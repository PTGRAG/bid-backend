package lub

import (
	"context"
	"fmt"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// ---------------------- Auction management (admin) ----------------------

// CreateAuction creates a new auction in draft status.
func (s *Service) CreateAuction(ctx context.Context, a *domain.Auction, createdBy uuid.UUID) error {
	a.Status = domain.AuctionStatusDraft
	a.CreatedBy = createdBy
	if a.MinBidValue.IsZero() {
		a.MinBidValue = domain.NewMoney(0.01)
	}
	if a.MinParticipants <= 0 {
		a.MinParticipants = defaultMinParticipants
	}
	// Free-entrance promo is chosen by the admin (limit 0 = "all"). A new auction never
	// starts with claimed slots, whatever the caller passed.
	if err := domain.ValidateFreeEntranceLimit(a.FreeEntranceLimit); err != nil {
		return domain.NewAppError(422, err.Error(), err)
	}
	a.FreeEntranceClaimed = 0
	if err := s.auctionRepo.Create(a); err != nil {
		return err
	}
	s.logAudit(createdBy, "auction.created", "auction", a.ID.String(),
		fmt.Sprintf("%s | %s", a.Title, describeFreeEntrance(a.FreeEntranceEnabled, a.FreeEntranceLimit)))
	return nil
}

// FreeEntranceSettings are the admin-controlled promo fields on an auction update.
// A nil field means "leave unchanged". Limit 0 means "all".
type FreeEntranceSettings struct {
	Enabled *bool
	Limit   *int
}

func describeFreeEntrance(enabled bool, limit int) string {
	if !enabled {
		return "free_entrance=off"
	}
	if limit == domain.FreeEntranceUnlimited {
		return "free_entrance=on limit=all"
	}
	return fmt.Sprintf("free_entrance=on limit=%d", limit)
}

// UpdateAuction updates mutable fields of an auction.
func (s *Service) UpdateAuction(ctx context.Context, id uuid.UUID, updates *domain.Auction, actorID uuid.UUID) error {
	return s.UpdateAuctionWithFreeEntrance(ctx, id, updates, nil, actorID)
}

// UpdateAuctionWithFreeEntrance updates mutable fields and (optionally) the free-entrance
// promo in a single validated write, so a rejected promo change never leaves a half-applied edit.
func (s *Service) UpdateAuctionWithFreeEntrance(
	ctx context.Context,
	id uuid.UUID,
	updates *domain.Auction,
	free *FreeEntranceSettings,
	actorID uuid.UUID,
) error {
	a, err := s.auctionRepo.GetByID(id)
	if err != nil {
		return err
	}
	if a.Status == domain.AuctionStatusExecuted || a.Status == domain.AuctionStatusCancelled {
		return domain.NewAppError(422, "Cannot edit an auction that is already executed or cancelled", nil)
	}
	freeChanged := false
	if free != nil {
		newEnabled, newLimit := a.FreeEntranceEnabled, a.FreeEntranceLimit
		if free.Enabled != nil {
			newEnabled = *free.Enabled
		}
		if free.Limit != nil {
			newLimit = *free.Limit
		}
		if err := domain.ValidateFreeEntranceLimit(newLimit); err != nil {
			return domain.NewAppError(422, err.Error(), err)
		}
		// Already-reserved slots are real customers: the cap may not drop below them.
		if newLimit != domain.FreeEntranceUnlimited && newLimit < a.FreeEntranceClaimed {
			return domain.NewAppError(422, fmt.Sprintf(
				"Free entrance limit cannot be lower than the %d spots already claimed", a.FreeEntranceClaimed), nil)
		}
		freeChanged = newEnabled != a.FreeEntranceEnabled || newLimit != a.FreeEntranceLimit
		a.FreeEntranceEnabled, a.FreeEntranceLimit = newEnabled, newLimit
	}
	if updates.Title != "" {
		a.Title = updates.Title
	}
	if updates.Description != "" {
		a.Description = updates.Description
	}
	if updates.ImageURLs != nil {
		a.ImageURLs = updates.ImageURLs
		if len(a.ImageURLs) > 0 {
			a.ImageURL = a.ImageURLs[0]
		} else {
			a.ImageURL = ""
		}
	} else if updates.ImageURL != "" {
		a.ImageURL = updates.ImageURL
		if len(a.ImageURLs) == 0 {
			a.ImageURLs = []string{updates.ImageURL}
		}
	}
	if updates.VideoURL != "" {
		a.VideoURL = updates.VideoURL
	}
	if updates.EntranceFee.IsPositive() && a.Status == domain.AuctionStatusDraft {
		a.EntranceFee = updates.EntranceFee
	}
	if updates.RetailValue.IsPositive() && a.Status == domain.AuctionStatusDraft {
		a.RetailValue = updates.RetailValue
	}
	if updates.MinParticipants > 0 && a.Status == domain.AuctionStatusDraft {
		a.MinParticipants = updates.MinParticipants
	}
	if updates.MinBidValue.IsPositive() && a.Status == domain.AuctionStatusDraft {
		a.MinBidValue = updates.MinBidValue
	}
	if updates.MaxBidValue != nil && a.Status == domain.AuctionStatusDraft {
		a.MaxBidValue = updates.MaxBidValue
	}
	if updates.StartAt != nil {
		a.StartAt = updates.StartAt
	}
	if updates.CloseAt != nil {
		a.CloseAt = updates.CloseAt
	}
	if err := s.auctionRepo.Update(a); err != nil {
		return err
	}
	detail := a.Title
	if freeChanged {
		detail += " | " + describeFreeEntrance(a.FreeEntranceEnabled, a.FreeEntranceLimit)
	}
	s.logAudit(actorID, "auction.updated", "auction", id.String(), detail)
	return nil
}

// ActivateAuction transitions an auction from draft → active.
func (s *Service) ActivateAuction(ctx context.Context, id uuid.UUID, actorID uuid.UUID) error {
	a, err := s.auctionRepo.GetByID(id)
	if err != nil {
		return err
	}
	if a.Status != domain.AuctionStatusDraft {
		return domain.NewAppError(422, "Only draft auctions can be activated", nil)
	}
	if !a.EntranceFee.IsPositive() {
		return domain.NewAppError(422, "Entrance fee must be set before activating", nil)
	}
	if a.CloseAt == nil {
		return domain.NewAppError(422, "CloseAt must be set before activating", nil)
	}
	if err := s.auctionRepo.UpdateStatus(id, domain.AuctionStatusActive); err != nil {
		return err
	}
	a.Status = domain.AuctionStatusActive
	s.logAudit(actorID, "auction.activated", "auction", id.String(), a.Title)
	s.notify(func() error {
		return s.notificationSvc.NotifyAuctionActivated(ctx, id, a.Title)
	})
	s.postTelegram(func() error {
		return s.telegramPoster.PostAuctionPublished(ctx, a)
	})
	return nil
}

// PostponeAuction extends the CloseAt of an active or postponed auction.
func (s *Service) PostponeAuction(ctx context.Context, id uuid.UUID, newCloseAt time.Time, actorID uuid.UUID) error {
	a, err := s.auctionRepo.GetByID(id)
	if err != nil {
		return err
	}
	if a.Status != domain.AuctionStatusActive && a.Status != domain.AuctionStatusPostponed {
		return domain.NewAppError(422, "Only active or postponed auctions can be postponed", nil)
	}
	now := time.Now().UTC()
	nc := newCloseAt.UTC()
	if !nc.After(now) {
		return domain.NewAppError(422, "New close time must be in the future", nil)
	}
	if a.CloseAt != nil && !nc.After(*a.CloseAt) {
		return domain.NewAppError(422, "New close time must be later than the current close time", nil)
	}
	a.CloseAt = &nc
	a.Status = domain.AuctionStatusPostponed
	if err := s.auctionRepo.Update(a); err != nil {
		return err
	}
	s.logAudit(actorID, "auction.postponed", "auction", id.String(),
		fmt.Sprintf("new_close_at=%s", nc.Format(time.RFC3339)))
	s.notify(func() error {
		return s.notificationSvc.NotifyAuctionPostponed(ctx, id, a.Title, nc.Format(time.RFC3339))
	})
	return nil
}

// CancelAuction cancels an active/draft auction.
func (s *Service) CancelAuction(ctx context.Context, id uuid.UUID, actorID uuid.UUID) error {
	a, err := s.auctionRepo.GetByID(id)
	if err != nil {
		return err
	}
	if a.Status == domain.AuctionStatusExecuted {
		return domain.NewAppError(422, "Cannot cancel an already executed auction", nil)
	}
	if a.Status == domain.AuctionStatusCancelled {
		return domain.NewAppError(422, "Auction is already cancelled", nil)
	}
	if err := s.auctionRepo.UpdateStatus(id, domain.AuctionStatusCancelled); err != nil {
		return err
	}
	s.logAudit(actorID, "auction.cancelled", "auction", id.String(), a.Title)
	s.notify(func() error {
		return s.notificationSvc.NotifyAuctionCancelled(ctx, id, a.Title, "")
	})
	return nil
}

// ---------------------- Participation (entrance fee) ----------------------

// RecordParticipation records that a participant paid the entrance fee.
// Idempotent on transactionID or existing active participation.
func (s *Service) RecordParticipation(
	ctx context.Context,
	auctionID uuid.UUID,
	participantID string,
	transactionID string,
	feePaid domain.Money,
) (*domain.AuctionParticipation, error) {
	return s.recordParticipation(ctx, auctionID, participantID, transactionID, feePaid, false)
}

// RecordFreeEntranceParticipation records a participation whose entrance fee was waived by the
// free-entrance promo. feePaid is what the customer actually paid (their bid amount(s) only).
// The slot was already reserved at checkout, so nothing is claimed here.
func (s *Service) RecordFreeEntranceParticipation(
	ctx context.Context,
	auctionID uuid.UUID,
	participantID string,
	transactionID string,
	feePaid domain.Money,
) (*domain.AuctionParticipation, error) {
	return s.recordParticipation(ctx, auctionID, participantID, transactionID, feePaid, true)
}

func (s *Service) recordParticipation(
	ctx context.Context,
	auctionID uuid.UUID,
	participantID string,
	transactionID string,
	feePaid domain.Money,
	entranceFree bool,
) (*domain.AuctionParticipation, error) {
	if transactionID != "" {
		if existing, _ := s.participationRepo.GetByTransactionID(transactionID); existing != nil {
			return existing, nil
		}
	}

	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return nil, err
	}
	if a.Status != domain.AuctionStatusActive && a.Status != domain.AuctionStatusPostponed {
		return nil, domain.NewAppError(422, "Auction is not currently accepting participants", nil)
	}
	if time.Now().UTC().After(*a.CloseAt) {
		return nil, domain.NewAppError(422, "Bidding period has closed for this auction", nil)
	}

	if existing, _ := s.participationRepo.GetByAuctionAndParticipant(auctionID, participantID); existing != nil {
		if existing.Status == domain.ParticipationStatusActive {
			return existing, nil
		}
	}

	part := &domain.AuctionParticipation{
		ID:            uuid.New(),
		AuctionID:     auctionID,
		ParticipantID: participantID,
		TransactionID: transactionID,
		FeePaid:       feePaid,
		EntranceFree:  entranceFree,
		Status:        domain.ParticipationStatusActive,
	}
	if err := s.participationRepo.Create(part); err != nil {
		return nil, err
	}
	return part, nil
}

// RefundParticipation marks a participation record as refunded.
func (s *Service) RefundParticipation(ctx context.Context, participationID uuid.UUID) error {
	return s.participationRepo.UpdateStatus(participationID, domain.ParticipationStatusRefunded)
}

// ---------------------- Bid management ----------------------

// ValidateAuctionAndBid checks that the auction is open for bidding and (if bidValue > 0) the bid value is valid and not a duplicate for this participant.
func (s *Service) ValidateAuctionAndBid(auctionID uuid.UUID, participantID string, bidValue domain.Money) (*domain.Auction, error) {
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return nil, err
	}
	if err := s.validateBiddingOpen(a); err != nil {
		return nil, err
	}
	if bidValue.IsPositive() {
		if err := s.validateBidValue(a, bidValue); err != nil {
			return nil, err
		}
		if participantID != "" {
			if existing, _ := s.bidRepo.GetByAuctionAndParticipantAndValue(auctionID, participantID, bidValue); existing != nil {
				return nil, domain.NewAppError(409, "You already have a bid with that value", nil)
			}
		}
	}
	return a, nil
}

// PlaceBid places a new secret bid for a participant.
// Each bid placed can be updated at most once.
func (s *Service) PlaceBid(
	ctx context.Context,
	auctionID uuid.UUID,
	participantID string,
	bidValue domain.Money,
) (*domain.Bid, error) {
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return nil, err
	}
	if err := s.validateBiddingOpen(a); err != nil {
		return nil, err
	}
	if err := s.validateBidValue(a, bidValue); err != nil {
		return nil, err
	}
	if err := s.requireParticipation(auctionID, participantID); err != nil {
		return nil, err
	}

	if existing, _ := s.bidRepo.GetByAuctionAndParticipantAndValue(auctionID, participantID, bidValue); existing != nil {
		return nil, domain.NewAppError(409, "You already have a bid with that value", nil)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	bid := &domain.Bid{
		ID:            uuid.New(),
		AuctionID:     auctionID,
		ParticipantID: participantID,
		BidValue:      bidValue,
		PlacedAt:      now,
		UpdatedAt:     now,
	}
	if err := s.bidRepo.Create(bid); err != nil {
		return nil, err
	}
	bid.PopulateUpdateState()
	s.notify(func() error {
		return s.notificationSvc.NotifyBidPlaced(ctx, participantID, auctionID, a.Title, bidValue)
	})
	return bid, nil
}

// UpdateBid changes the bid_value on an existing Bid (allowed at most once).
func (s *Service) UpdateBid(
	ctx context.Context,
	bidID uuid.UUID,
	participantID string,
	newBidValue domain.Money,
) (*domain.Bid, error) {
	bid, err := s.bidRepo.GetByID(bidID)
	if err != nil {
		return nil, err
	}
	if bid.ParticipantID != participantID {
		return nil, domain.ErrForbidden
	}
	if bid.HasBeenUpdated() {
		return nil, domain.NewAppError(422, "You can only update your bid one time", nil)
	}

	a, err := s.auctionRepo.GetByID(bid.AuctionID)
	if err != nil {
		return nil, err
	}
	if err := s.validateBiddingOpen(a); err != nil {
		return nil, err
	}
	if err := s.validateBidValue(a, newBidValue); err != nil {
		return nil, err
	}
	if bid.BidValue.Equal(newBidValue) {
		return nil, domain.NewAppError(422, "New bid value must be different from your current bid", nil)
	}

	if existing, _ := s.bidRepo.GetByAuctionAndParticipantAndValue(bid.AuctionID, participantID, newBidValue); existing != nil {
		if existing.ID != bidID {
			return nil, domain.NewAppError(409, "You already have a bid with that value", nil)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(bid.PlacedAt.Add(100 * time.Millisecond)) {
		now = bid.PlacedAt.Add(time.Second)
	}
	oldValue := bid.BidValue
	bid.BidValue = newBidValue
	bid.UpdatedAt = now
	if err := s.bidRepo.Update(bid); err != nil {
		return nil, err
	}
	bid.PopulateUpdateState()
	s.notify(func() error {
		return s.notificationSvc.NotifyBidUpdated(ctx, participantID, bid.AuctionID, a.Title, oldValue, newBidValue)
	})
	return bid, nil
}

// ---------------------- Helpers ----------------------

func (s *Service) validateBiddingOpen(a *domain.Auction) error {
	if a.Status != domain.AuctionStatusActive && a.Status != domain.AuctionStatusPostponed {
		return domain.NewAppError(422, "This auction is not currently accepting bids", nil)
	}
	if a.CloseAt != nil && time.Now().UTC().After(*a.CloseAt) {
		return domain.NewAppError(422, "The bidding period for this auction has closed", nil)
	}
	return nil
}

func (s *Service) validateBidValue(a *domain.Auction, v domain.Money) error {
	if v.LessThan(a.MinBidValue) {
		return domain.NewAppError(422, fmt.Sprintf("Bid value must be at least %.2f", a.MinBidValue.Float64()), nil)
	}
	if a.MaxBidValue != nil && a.MaxBidValue.LessThan(v) {
		return domain.NewAppError(422, fmt.Sprintf("Bid value must not exceed %.2f", a.MaxBidValue.Float64()), nil)
	}
	return nil
}

func (s *Service) requireParticipation(auctionID uuid.UUID, participantID string) error {
	part, err := s.participationRepo.GetByAuctionAndParticipant(auctionID, participantID)
	if err != nil {
		return err
	}
	if part == nil || part.Status != domain.ParticipationStatusActive {
		return domain.NewAppError(402, "You must pay the entrance fee before placing a bid", nil)
	}
	return nil
}

func (s *Service) logAudit(actorID uuid.UUID, action, entity, entityID, value string) {
	if s.audit == nil {
		return
	}
	s.audit.Log(&domain.AuditLog{
		UserID:   actorID,
		Action:   action,
		Entity:   entity,
		EntityID: entityID,
		NewValue: value,
	}, nil)
}
