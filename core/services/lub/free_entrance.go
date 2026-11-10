package lub

import (
	"strings"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// FreeEntranceStatus reports promo availability for an auction + optional viewer.
//
// userEligible is true only when all of these hold:
//   - the admin enabled the promo on this auction and a free slot is still available
//     (always available when the limit is "all");
//   - the viewer is logged in and has never used a free entrance on any auction;
//   - the viewer has not already joined this auction.
//
// remaining is the number of free spots left for a finite limit (0 when unlimited, check
// the auction's FreeEntranceIsUnlimited).
func (s *Service) FreeEntranceStatus(auctionID uuid.UUID, participantID string) (remaining int, userEligible bool, err error) {
	a, err := s.auctionRepo.GetByID(auctionID)
	if err != nil {
		return 0, false, err
	}
	remaining = a.FreeSlotsRemaining()
	if !a.FreeEntranceAvailable() || strings.TrimSpace(participantID) == "" {
		return remaining, false, nil
	}
	eligible, err := s.viewerQualifiesForFreeEntrance(auctionID, participantID)
	if err != nil {
		return remaining, false, err
	}
	return remaining, eligible, nil
}

// viewerQualifiesForFreeEntrance applies the per-customer rules (once ever, not already joined).
func (s *Service) viewerQualifiesForFreeEntrance(auctionID uuid.UUID, participantID string) (bool, error) {
	used, err := s.participationRepo.HasUsedFreeEntrance(participantID)
	if err != nil {
		return false, err
	}
	if used {
		return false, nil
	}
	existing, err := s.participationRepo.GetByAuctionAndParticipant(auctionID, participantID)
	if err != nil {
		return false, err
	}
	if existing != nil && existing.Status == domain.ParticipationStatusActive {
		return false, nil
	}
	return true, nil
}

// EnrichAuctionFreeEntrance fills computed free-entrance fields for API responses.
// Lookup failures degrade to "not eligible" so the UI never promises a free entrance the
// server could not confirm (checkout re-validates anyway).
func (s *Service) EnrichAuctionFreeEntrance(a *domain.Auction, viewerID string) {
	if a == nil {
		return
	}
	a.FreeEntranceRemaining = a.FreeSlotsRemaining()
	a.FreeEntranceUnlimited = a.FreeEntranceIsUnlimited()
	a.UserEligibleForFreeEntrance = false
	if !a.FreeEntranceAvailable() || strings.TrimSpace(viewerID) == "" || a.UserHasJoined {
		return
	}
	eligible, err := s.viewerQualifiesForFreeEntrance(a.ID, viewerID)
	if err != nil {
		return
	}
	a.UserEligibleForFreeEntrance = eligible
}
