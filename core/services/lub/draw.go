package lub

import (
	"context"
	"fmt"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// DrawResult contains the outcome of executing an auction draw.
type DrawResult struct {
	WinnerBidID       uuid.UUID
	WinnerParticipant string
	WinnerBidValue    domain.Money
	HasWinner         bool // false when every bid value was placed by >1 participant
}

// ExecuteDraw finds the lowest unique bid and marks it as the winner.
func (s *Service) ExecuteDraw(ctx context.Context, auctionID uuid.UUID, trigger string, actorID uuid.UUID) (*DrawResult, error) {
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return nil, err
	}

	switch a.Status {
	case domain.AuctionStatusActive, domain.AuctionStatusPostponed, domain.AuctionStatusClosed:
		// allowed
	case domain.AuctionStatusExecuted:
		return nil, domain.NewAppError(409, "Draw has already been executed for this auction", nil)
	case domain.AuctionStatusCancelled:
		return nil, domain.NewAppError(422, "Cannot execute draw on a cancelled auction", nil)
	default:
		return nil, domain.NewAppError(422, fmt.Sprintf("Auction status %q does not allow draw execution", a.Status), nil)
	}

	winningBid, err := s.bidRepo.FindWinner(auctionID)
	if err != nil {
		return nil, fmt.Errorf("draw: find winner: %w", err)
	}

	if a.Status != domain.AuctionStatusClosed {
		if err := s.auctionRepo.UpdateStatus(auctionID, domain.AuctionStatusClosed); err != nil {
			return nil, err
		}
	}

	result := &DrawResult{}

	if winningBid == nil {
		s.logAudit(actorID, "auction.draw.no_winner",
			"auction", auctionID.String(),
			fmt.Sprintf("trigger=%s, no unique bid found", trigger))
		s.notify(func() error {
			return s.notificationSvc.NotifyAuctionNoWinner(ctx, auctionID, a.Title)
		})
		s.alertAdmin(ctx, fmt.Sprintf("🤷 Draw found no unique bid\nauction: %s (%s)", a.Title, auctionID.String()))
		return result, nil
	}

	if err := s.bidRepo.MarkWinner(auctionID, winningBid.ID); err != nil {
		return nil, fmt.Errorf("draw: mark winner: %w", err)
	}

	now := time.Now().UTC()
	a.WinnerBidID = &winningBid.ID
	a.WinnerParticipantID = &winningBid.ParticipantID
	a.WinnerBidValue = &winningBid.BidValue

	// Pay only if you win: open the winner's payment offer before the auction is marked executed,
	// so a failure here leaves it "closed" and the draw can simply be run again.
	if s.winnerOffers != nil {
		if _, err := s.openWinnerOffer(ctx, a, winningBid, 1); err != nil {
			return nil, fmt.Errorf("draw: %w", err)
		}
	}

	a.ExecutedAt = &now
	a.Status = domain.AuctionStatusExecuted

	if err := s.auctionRepo.Update(a); err != nil {
		return nil, fmt.Errorf("draw: save auction winner: %w", err)
	}

	result.HasWinner = true
	result.WinnerBidID = winningBid.ID
	result.WinnerParticipant = winningBid.ParticipantID
	result.WinnerBidValue = winningBid.BidValue

	s.logAudit(actorID, "auction.draw.executed",
		"auction", auctionID.String(),
		fmt.Sprintf("trigger=%s winner=%s bid_value=%.2f",
			trigger, winningBid.ParticipantID, winningBid.BidValue.Float64()))

	if s.winnerOffers == nil {
		s.notify(func() error {
			return s.notificationSvc.NotifyAuctionWinner(ctx, winningBid.ParticipantID, auctionID, a.Title, winningBid.BidValue)
		})
	}
	if n := s.winnerNotifier(); n != nil {
		s.notify(func() error {
			return n.NotifyAuctionResultLosers(ctx, auctionID, a.Title, winningBid.ParticipantID)
		})
	}

	return result, nil
}

// CloseAuction transitions an active/postponed auction to "closed" without executing the draw.
func (s *Service) CloseAuction(ctx context.Context, auctionID uuid.UUID, actorID uuid.UUID) error {
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return err
	}
	if a.Status != domain.AuctionStatusActive && a.Status != domain.AuctionStatusPostponed {
		return domain.NewAppError(422, "Only active or postponed auctions can be closed", nil)
	}
	if err := s.auctionRepo.UpdateStatus(auctionID, domain.AuctionStatusClosed); err != nil {
		return err
	}
	s.logAudit(actorID, "auction.closed", "auction", auctionID.String(), a.Title)
	s.notify(func() error {
		return s.notificationSvc.NotifyAuctionClosed(ctx, auctionID, a.Title)
	})
	return nil
}

// ProcessDueAuctions is called by the scheduler to auto-close auctions whose CloseAt has passed.
func (s *Service) ProcessDueAuctions(ctx context.Context) error {
	auctions, err := s.auctionRepo.ListDueForClose()
	if err != nil {
		return err
	}
	for _, a := range auctions {
		aid := a.ID
		if _, err := s.ExecuteDraw(ctx, aid, "scheduled", uuid.Nil); err != nil {
			s.logAudit(uuid.Nil, "auction.draw.auto_error",
				"auction", aid.String(), err.Error())
		}
	}
	return nil
}
