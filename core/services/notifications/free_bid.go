package notifications

import (
	"context"
	"fmt"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// NotifyNearMissFreeBid tells a customer whose bid came close that they got one free bid. In-app
// always; Telegram DM and email only for customers who opted in to result messages. No SMS.
func (s *Service) NotifyNearMissFreeBid(ctx context.Context, participantID string, auctionID uuid.UUID, title string, reason string, rank int) error {
	uid, err := uuid.Parse(participantID)
	if err != nil {
		return nil
	}
	name := displayName(title)
	why := "your bid was one of the closest to winning"
	switch {
	case reason == domain.NearMissRunnerUp && rank > 1:
		why = fmt.Sprintf("yours was the %s lowest unique bid", ordinalWord(rank))
	case reason == domain.NearMissDuplicateKnockout:
		why = "your bid was below the winning bid but someone picked the same number"
	}
	msg := fmt.Sprintf("So close on '%s': %s. We've added 1 free bid to your account. It covers the entrance fee of your next ticket on any auction.", name, why)
	aid := auctionID
	n := &domain.Notification{
		ID:          uuid.New(),
		UserID:      uid,
		UserType:    domain.UserTypeParticipant,
		Type:        domain.NotificationTypeFreeBid,
		Title:       "You earned a free bid",
		Message:     msg,
		ActionURL:   "/auctions",
		ActionLabel: "Use it now",
		AuctionID:   &aid,
		Priority:    domain.NotificationPriorityNormal,
	}
	if err := s.flushBatch(ctx, []*domain.Notification{n}); err != nil {
		return err
	}
	if s.userRepo == nil {
		return nil
	}
	u, err := s.userRepo.GetByID(uid)
	if err != nil || u == nil {
		return nil
	}
	if u.NotifyResultsTelegram {
		s.sendParticipantTelegram(ctx, participantID, "🎁 "+msg, "Use it now")
	}
	if u.NotifyResultsEmail {
		s.sendParticipantEmail(participantID, "You earned a free bid", "You earned a free bid", msg, "Use it now",
			s.absoluteURL("/auctions"))
	}
	return nil
}

func ordinalWord(n int) string {
	if n%10 == 1 && n%100 != 11 {
		return fmt.Sprintf("%dst", n)
	}
	if n%10 == 2 && n%100 != 12 {
		return fmt.Sprintf("%dnd", n)
	}
	if n%10 == 3 && n%100 != 13 {
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}
