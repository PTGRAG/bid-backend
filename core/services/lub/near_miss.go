package lub

import (
	"context"
	"fmt"
	"strings"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

// Near-miss free bids: once the winner has paid (the result is locked), the closest losing bids
// get one free bid each. See domain.SelectNearMisses for who qualifies.

// NearMissNotifier tells a customer they earned a free bid. The notifications service implements it.
type NearMissNotifier interface {
	NotifyNearMissFreeBid(ctx context.Context, participantID string, auctionID uuid.UUID, title string, reason string, rank int) error
}

// SetFreeBids enables near-miss free bids.
func (s *Service) SetFreeBids(repo ports.FreeBidRepository) { s.freeBids = repo }

// maxAuctionBids bounds the bids read when settling one auction.
const maxAuctionBids = 100000

// CreditNearMisses gives the near-miss free bids for an auction whose winner has paid. It is safe
// to run more than once: customers already credited for the auction are skipped.
func (s *Service) CreditNearMisses(ctx context.Context, auctionID uuid.UUID, actorID uuid.UUID) ([]domain.FreeBidGrant, error) {
	if s.freeBids == nil || s.winnerOffers == nil {
		return nil, nil
	}
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, domain.NewAppError(404, "Auction not found", nil)
	}
	offers, err := s.winnerOffers.ListByAuction(auctionID)
	if err != nil {
		return nil, err
	}
	var paid *domain.WinnerOffer
	forfeited := make([]string, 0, len(offers))
	for i := range offers {
		switch offers[i].Status {
		case domain.WinnerOfferPaid:
			paid = &offers[i]
		case domain.WinnerOfferExpired:
			forfeited = append(forfeited, offers[i].ParticipantID)
		}
	}
	if paid == nil {
		return nil, domain.NewAppError(409, "Free bids are credited only after the winner has paid", nil)
	}

	settings, err := s.freeBids.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	bids, _, err := s.bidRepo.ListByAuction(auctionID, maxAuctionBids, 0)
	if err != nil {
		return nil, err
	}
	in := make([]domain.NearMissBid, 0, len(bids))
	for _, b := range bids {
		in = append(in, domain.NearMissBid{ParticipantID: b.ParticipantID, BidValue: b.BidValue})
	}
	picks := domain.SelectNearMisses(in, paid.ParticipantID, paid.BidValue, forfeited, settings)
	if len(picks) == 0 {
		return nil, nil
	}

	var creator *uuid.UUID
	if actorID != uuid.Nil {
		creator = &actorID
	}
	grants := make([]domain.FreeBidGrant, 0, len(picks))
	for _, p := range picks {
		v := p.BidValue
		aid := auctionID
		grants = append(grants, domain.FreeBidGrant{
			ParticipantID: p.ParticipantID,
			AuctionID:     &aid,
			Reason:        p.Reason,
			Rank:          p.Rank,
			BidValue:      &v,
			CreatedBy:     creator,
		})
	}
	created, err := s.freeBids.CreateGrants(ctx, grants)
	if err != nil {
		return created, fmt.Errorf("credit near-miss free bids: %w", err)
	}
	if len(created) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(created))
	for _, g := range created {
		ids = append(ids, g.ParticipantID)
	}
	s.logAudit(actorID, "auction.near_miss.credited", "auction", auctionID.String(),
		fmt.Sprintf("count=%d participants=%s", len(created), strings.Join(ids, ",")))
	if n, ok := s.notificationSvc.(NearMissNotifier); ok {
		for _, g := range created {
			g := g
			s.notify(func() error {
				return n.NotifyNearMissFreeBid(ctx, g.ParticipantID, auctionID, a.Title, g.Reason, g.Rank)
			})
		}
	}
	return created, nil
}

// creditNearMissesAfterPayment runs right after the winner's payment is recorded. Failures are
// logged for staff, who can re-run the credit from the auction page; they never fail the payment.
func (s *Service) creditNearMissesAfterPayment(ctx context.Context, auctionID uuid.UUID, title string) {
	if s.freeBids == nil {
		return
	}
	if _, err := s.CreditNearMisses(ctx, auctionID, uuid.Nil); err != nil {
		s.logAudit(uuid.Nil, "auction.near_miss.failed", "auction", auctionID.String(), err.Error())
		s.alertAdmin(ctx, fmt.Sprintf(
			"⚠️ Near-miss free bids were not credited\nauction: %s (%s)\nerror: %s\nRe-run from the auction's Free bids tab.",
			title, auctionID.String(), err.Error()))
	}
}

// NearMissSettings returns the current near-miss rules.
func (s *Service) NearMissSettings(ctx context.Context) (domain.NearMissSettings, error) {
	if s.freeBids == nil {
		return domain.DefaultNearMissSettings(), nil
	}
	return s.freeBids.GetSettings(ctx)
}

// UpdateNearMissSettings validates and stores the near-miss rules.
func (s *Service) UpdateNearMissSettings(ctx context.Context, in domain.NearMissSettings, actorID uuid.UUID) (domain.NearMissSettings, error) {
	if s.freeBids == nil {
		return in, fmt.Errorf("free bids are not configured")
	}
	if err := in.Validate(); err != nil {
		return in, err
	}
	if actorID != uuid.Nil {
		in.UpdatedBy = &actorID
	}
	if err := s.freeBids.SaveSettings(ctx, in); err != nil {
		return in, err
	}
	s.logAudit(actorID, "near_miss.settings.updated", "near_miss_settings", "1",
		fmt.Sprintf("enabled=%t runner_ups=%d include_duplicates=%t max_duplicates=%d",
			in.Enabled, in.RunnerUps, in.IncludeDuplicates, in.MaxDuplicates))
	return s.freeBids.GetSettings(ctx)
}

// ListNearMissGrants returns the free bids given for an auction (staff).
func (s *Service) ListNearMissGrants(ctx context.Context, auctionID uuid.UUID) ([]domain.FreeBidGrant, error) {
	if s.freeBids == nil {
		return nil, nil
	}
	return s.freeBids.ListByAuction(ctx, auctionID)
}

// FreeBidBalance returns a customer's free-bid balance and most recent grants.
func (s *Service) FreeBidBalance(ctx context.Context, participantID string) (domain.FreeBidBalance, error) {
	out := domain.FreeBidBalance{Recent: []domain.FreeBidGrant{}}
	if s.freeBids == nil || strings.TrimSpace(participantID) == "" {
		return out, nil
	}
	available, earned, used, err := s.freeBids.Balance(ctx, participantID)
	if err != nil {
		return out, err
	}
	out.Available, out.Earned, out.Used = available, earned, used
	recent, err := s.freeBids.ListByParticipant(ctx, participantID, 10)
	if err != nil {
		return out, err
	}
	if recent != nil {
		out.Recent = recent
	}
	return out, nil
}
