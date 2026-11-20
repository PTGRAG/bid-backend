package lub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

// Pay only if you win: after the draw the winner is offered the prize and has
// domain.WinnerPaymentWindow to pay their bid amount. See domain.WinnerOffer.

// WinnerNotifier sends the pay-only-if-you-win messages. The notifications service implements it.
type WinnerNotifier interface {
	// NotifyWinnerPaymentDue tells a (new) winner they won and must pay bidValue by dueAt, on every
	// channel (in-app, Telegram DM, SMS, email). attempt > 1 means an earlier winner did not pay.
	NotifyWinnerPaymentDue(ctx context.Context, winnerID string, auctionID uuid.UUID, title string, bidValue domain.Money, dueAt time.Time, attempt int) error
	// NotifyWinnerPaymentReminder reminds the winner (reminder 1 or 2) on every channel.
	NotifyWinnerPaymentReminder(ctx context.Context, winnerID string, auctionID uuid.UUID, title string, bidValue domain.Money, dueAt time.Time, reminder int) error
	// NotifyWinnerPaymentExpired tells a winner their payment deadline passed.
	NotifyWinnerPaymentExpired(ctx context.Context, participantID string, auctionID uuid.UUID, title string) error
	// NotifyWinnerPaid confirms the winner's payment.
	NotifyWinnerPaid(ctx context.Context, winnerID string, auctionID uuid.UUID, title string, amount domain.Money, txRef string) error
	// NotifyAuctionResultLosers tells every other participant the draw is done (in-app always,
	// email / Telegram DM only for users who opted in). No SMS.
	NotifyAuctionResultLosers(ctx context.Context, auctionID uuid.UUID, title string, winnerID string) error
}

// ParticipantContactLookup returns a participant's name and phone for staff alerts.
type ParticipantContactLookup interface {
	ParticipantContact(participantID string) (name, phone, email string)
}

// WinnerPaymentChecker reports whether a winner payment for the offer is still being confirmed
// (started but not yet settled by Chapa). The payment ledger implements it.
type WinnerPaymentChecker interface {
	HasPendingWinnerPayment(ctx context.Context, offerID uuid.UUID) (bool, error)
}

// SetWinnerOffers enables pay-only-if-you-win. Without it the draw only announces the winner.
func (s *Service) SetWinnerOffers(repo ports.WinnerOfferRepository) { s.winnerOffers = repo }

// SetAdminNotifier sends staff alerts (Telegram admin chat).
func (s *Service) SetAdminNotifier(n ports.AdminNotifier) { s.admin = n }

// SetWinnerPaymentChecker lets the deadline check wait for a payment Chapa is still confirming.
func (s *Service) SetWinnerPaymentChecker(c WinnerPaymentChecker) { s.paymentChecker = c }

// SetContactLookup lets staff alerts include the winner's name and phone.
func (s *Service) SetContactLookup(c ParticipantContactLookup) { s.contacts = c }

// SetClock overrides the current time (tests only).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) winnerNotifier() WinnerNotifier {
	if s.notificationSvc == nil {
		return nil
	}
	if n, ok := s.notificationSvc.(WinnerNotifier); ok {
		return n
	}
	return nil
}

func (s *Service) alertAdmin(ctx context.Context, text string) {
	if s.admin == nil {
		return
	}
	s.admin.NotifyAdmin(ctx, text)
}

func (s *Service) describeParticipant(participantID string) string {
	if s.contacts == nil {
		return participantID
	}
	name, phone, email := s.contacts.ParticipantContact(participantID)
	parts := []string{}
	if strings.TrimSpace(name) != "" {
		parts = append(parts, name)
	}
	if strings.TrimSpace(phone) != "" {
		parts = append(parts, "phone "+phone)
	}
	if strings.TrimSpace(email) != "" {
		parts = append(parts, email)
	}
	if len(parts) == 0 {
		return participantID
	}
	return strings.Join(parts, ", ") + " (" + participantID + ")"
}

func formatDue(t time.Time) string {
	// Ethiopia is UTC+3 all year.
	return t.In(time.FixedZone("EAT", 3*60*60)).Format("Mon 2 Jan 15:04 EAT")
}

// openWinnerOffer records a new "pay your bid" offer for bid and notifies the winner and staff.
func (s *Service) openWinnerOffer(ctx context.Context, a *domain.Auction, bid *domain.Bid, attempt int) (*domain.WinnerOffer, error) {
	now := s.clock()
	offer := &domain.WinnerOffer{
		ID:            uuid.New(),
		AuctionID:     a.ID,
		BidID:         bid.ID,
		ParticipantID: bid.ParticipantID,
		BidValue:      bid.BidValue,
		Attempt:       attempt,
		Status:        domain.WinnerOfferAwaitingPayment,
		DueAt:         now.Add(domain.WinnerPaymentWindow),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.winnerOffers.Create(offer); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// Another worker opened it already (draw re-run / two replicas): use theirs.
			existing, gErr := s.winnerOffers.GetOpen(a.ID)
			if gErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("open winner offer: %w", err)
	}

	s.logAudit(uuid.Nil, "auction.winner_offer.opened", "auction", a.ID.String(),
		fmt.Sprintf("attempt=%d participant=%s bid_value=%s due_at=%s",
			attempt, bid.ParticipantID, bid.BidValue.String(), offer.DueAt.Format(time.RFC3339)))

	if n := s.winnerNotifier(); n != nil {
		s.notify(func() error {
			return n.NotifyWinnerPaymentDue(ctx, bid.ParticipantID, a.ID, a.Title, bid.BidValue, offer.DueAt, attempt)
		})
	}

	header := "🏆 Winner drawn"
	if attempt > 1 {
		header = fmt.Sprintf("🔁 Prize passed to the next lowest unique bid (offer %d of %d)", attempt, domain.MaxWinnerAttempts)
	}
	s.alertAdmin(ctx, fmt.Sprintf(
		"%s\nauction: %s (%s)\nwinner: %s\nbid to pay: %s ETB\npay by: %s",
		header, a.Title, a.ID.String(), s.describeParticipant(bid.ParticipantID),
		bid.BidValue.String(), formatDue(offer.DueAt),
	))
	return offer, nil
}

// GetOpenWinnerOffer returns the auction's offer that still waits for payment, or nil.
func (s *Service) GetOpenWinnerOffer(auctionID uuid.UUID) (*domain.WinnerOffer, error) {
	if s.winnerOffers == nil {
		return nil, nil
	}
	return s.winnerOffers.GetOpen(auctionID)
}

// GetWinnerOffer returns one offer by ID.
func (s *Service) GetWinnerOffer(id uuid.UUID) (*domain.WinnerOffer, error) {
	if s.winnerOffers == nil {
		return nil, domain.ErrNotFound
	}
	return s.winnerOffers.GetByID(id)
}

// ListWinnerOffers returns every offer made for an auction (staff view), oldest first.
func (s *Service) ListWinnerOffers(auctionID uuid.UUID) ([]domain.WinnerOffer, error) {
	if s.winnerOffers == nil {
		return []domain.WinnerOffer{}, nil
	}
	offers, err := s.winnerOffers.ListByAuction(auctionID)
	if err != nil {
		return nil, err
	}
	if s.contacts != nil {
		for i := range offers {
			offers[i].ParticipantName, offers[i].ParticipantPhone, offers[i].ParticipantEmail =
				s.contacts.ParticipantContact(offers[i].ParticipantID)
		}
	}
	return offers, nil
}

// RequirePayableOffer returns the open offer the participant may pay right now.
func (s *Service) RequirePayableOffer(auctionID uuid.UUID, participantID string) (*domain.WinnerOffer, error) {
	offer, err := s.GetOpenWinnerOffer(auctionID)
	if err != nil {
		return nil, err
	}
	if offer == nil || !strings.EqualFold(offer.ParticipantID, participantID) {
		return nil, domain.NewAppError(403, "There is no winning bid for you to pay on this auction", nil)
	}
	if !offer.CanPayAt(s.clock()) {
		return nil, domain.NewAppError(422, "The payment deadline for this win has passed", nil)
	}
	return offer, nil
}

// MarkWinnerOfferPaid records the winner's payment. ok=false means the offer was no longer
// awaiting payment (it expired and the prize moved on): staff must refund txRef.
func (s *Service) MarkWinnerOfferPaid(ctx context.Context, offerID uuid.UUID, txRef string, amount domain.Money) (bool, error) {
	if s.winnerOffers == nil {
		return false, fmt.Errorf("winner offers are not configured")
	}
	offer, err := s.winnerOffers.GetByID(offerID)
	if err != nil {
		return false, err
	}
	if offer.Status == domain.WinnerOfferPaid && offer.PaymentTxRef == txRef {
		return true, nil // already recorded (webhook + return URL)
	}
	ok, err := s.winnerOffers.MarkPaid(offerID, txRef, s.clock())
	if err != nil {
		return false, err
	}
	title := ""
	if a, aErr := s.auctionRepo.GetByID(offer.AuctionID); aErr == nil && a != nil {
		title = a.Title
	}
	if !ok {
		s.logAudit(uuid.Nil, "auction.winner_offer.late_payment", "auction", offer.AuctionID.String(),
			fmt.Sprintf("offer=%s tx_ref=%s status=%s", offerID, txRef, offer.Status))
		s.alertAdmin(ctx, fmt.Sprintf(
			"⚠️ Winner payment received for an offer that is no longer open (%s). Refund needed.\nauction: %s (%s)\nparticipant: %s\namount: %s ETB\ntx_ref: %s",
			offer.Status, title, offer.AuctionID.String(), s.describeParticipant(offer.ParticipantID), amount.String(), txRef,
		))
		return false, nil
	}
	s.logAudit(uuid.Nil, "auction.winner_offer.paid", "auction", offer.AuctionID.String(),
		fmt.Sprintf("offer=%s attempt=%d tx_ref=%s amount=%s", offerID, offer.Attempt, txRef, amount.String()))
	if n := s.winnerNotifier(); n != nil {
		s.notify(func() error {
			return n.NotifyWinnerPaid(ctx, offer.ParticipantID, offer.AuctionID, title, amount, txRef)
		})
	}
	s.alertAdmin(ctx, fmt.Sprintf(
		"💰 Winner paid. Arrange delivery.\nauction: %s (%s)\nwinner: %s\npaid: %s ETB\ntx_ref: %s",
		title, offer.AuctionID.String(), s.describeParticipant(offer.ParticipantID), amount.String(), txRef,
	))
	return true, nil
}

// ProcessWinnerPayments is run by the scheduler: it sends the two payment reminders and, once the
// deadline has passed, expires the offer and passes the prize to the next lowest unique bid.
func (s *Service) ProcessWinnerPayments(ctx context.Context) error {
	if s.winnerOffers == nil {
		return nil
	}
	offers, err := s.winnerOffers.ListOpen(500)
	if err != nil {
		return err
	}
	var firstErr error
	for i := range offers {
		if err := s.processOpenOffer(ctx, &offers[i]); err != nil {
			s.logAudit(uuid.Nil, "auction.winner_offer.process_error", "auction", offers[i].AuctionID.String(), err.Error())
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Service) processOpenOffer(ctx context.Context, offer *domain.WinnerOffer) error {
	now := s.clock()
	a, err := s.auctionRepo.GetByID(offer.AuctionID)
	if err != nil {
		return err
	}

	if now.Before(offer.DueAt) {
		// A late scheduler may owe both reminders: send them in order, one per pass.
		switch offer.ReminderDue(now) {
		case 1:
			return s.sendReminder(ctx, a, offer, 1)
		case 2:
			return s.sendReminder(ctx, a, offer, 2)
		}
		return nil
	}

	// Deadline passed. Wait a little for a payment Chapa is still confirming.
	if s.paymentChecker != nil && now.Before(offer.DueAt.Add(domain.WinnerPaymentGrace)) {
		pending, pErr := s.paymentChecker.HasPendingWinnerPayment(ctx, offer.ID)
		if pErr != nil {
			return pErr
		}
		if pending {
			return nil
		}
	}

	ok, err := s.winnerOffers.MarkExpired(offer.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // paid or handled by another worker in the meantime
	}
	s.logAudit(uuid.Nil, "auction.winner_offer.expired", "auction", a.ID.String(),
		fmt.Sprintf("offer=%s attempt=%d participant=%s", offer.ID, offer.Attempt, offer.ParticipantID))
	if n := s.winnerNotifier(); n != nil {
		s.notify(func() error {
			return n.NotifyWinnerPaymentExpired(ctx, offer.ParticipantID, a.ID, a.Title)
		})
	}
	s.alertAdmin(ctx, fmt.Sprintf(
		"⌛ Winner did not pay in time\nauction: %s (%s)\nwinner: %s\nbid: %s ETB (offer %d of %d)",
		a.Title, a.ID.String(), s.describeParticipant(offer.ParticipantID), offer.BidValue.String(),
		offer.Attempt, domain.MaxWinnerAttempts,
	))
	return s.passPrizeOn(ctx, a, offer)
}

func (s *Service) sendReminder(ctx context.Context, a *domain.Auction, offer *domain.WinnerOffer, reminder int) error {
	ok, err := s.winnerOffers.AdvanceReminder(offer.ID, reminder-1)
	if err != nil || !ok {
		return err
	}
	if n := s.winnerNotifier(); n != nil {
		s.notify(func() error {
			return n.NotifyWinnerPaymentReminder(ctx, offer.ParticipantID, a.ID, a.Title, offer.BidValue, offer.DueAt, reminder)
		})
	}
	if reminder == 2 {
		s.alertAdmin(ctx, fmt.Sprintf(
			"📞 Winner has not paid yet. Please call them.\nauction: %s (%s)\nwinner: %s\nbid to pay: %s ETB\ndeadline: %s",
			a.Title, a.ID.String(), s.describeParticipant(offer.ParticipantID), offer.BidValue.String(), formatDue(offer.DueAt),
		))
	}
	return nil
}

// passPrizeOn offers the prize to the next lowest unique bid, or cancels the auction when the
// attempts are used up or no other unique bid is left.
func (s *Service) passPrizeOn(ctx context.Context, a *domain.Auction, expired *domain.WinnerOffer) error {
	if expired.Attempt >= domain.MaxWinnerAttempts {
		return s.cancelUnpaidAuction(ctx, a, fmt.Sprintf("%d winners in a row did not pay", domain.MaxWinnerAttempts))
	}
	offers, err := s.winnerOffers.ListByAuction(a.ID)
	if err != nil {
		return err
	}
	exclude := make([]string, 0, len(offers))
	for _, o := range offers {
		exclude = append(exclude, o.ParticipantID)
	}
	next, err := s.bidRepo.FindWinnerExcluding(a.ID, exclude)
	if err != nil {
		return fmt.Errorf("find next winner: %w", err)
	}
	if next == nil {
		return s.cancelUnpaidAuction(ctx, a, "no other unique bid is left")
	}
	if err := s.bidRepo.MoveWinner(a.ID, next.ID); err != nil {
		return fmt.Errorf("move winner: %w", err)
	}
	a.WinnerBidID = &next.ID
	a.WinnerParticipantID = &next.ParticipantID
	a.WinnerBidValue = &next.BidValue
	if err := s.auctionRepo.Update(a); err != nil {
		return fmt.Errorf("save next winner: %w", err)
	}
	s.logAudit(uuid.Nil, "auction.winner.passed_on", "auction", a.ID.String(),
		fmt.Sprintf("from=%s to=%s bid_value=%s attempt=%d", expired.ParticipantID, next.ParticipantID, next.BidValue.String(), expired.Attempt+1))
	_, err = s.openWinnerOffer(ctx, a, next, expired.Attempt+1)
	return err
}

func (s *Service) cancelUnpaidAuction(ctx context.Context, a *domain.Auction, reason string) error {
	if err := s.auctionRepo.UpdateStatus(a.ID, domain.AuctionStatusCancelled); err != nil {
		return err
	}
	s.logAudit(uuid.Nil, "auction.cancelled.unpaid", "auction", a.ID.String(), reason)
	s.notify(func() error {
		return s.notificationSvc.NotifyAuctionCancelled(ctx, a.ID, a.Title, "the winning bid was not paid in time")
	})
	s.alertAdmin(ctx, fmt.Sprintf("🚫 Auction cancelled: no winner paid\nauction: %s (%s)\nreason: %s", a.Title, a.ID.String(), reason))
	return nil
}

// ---------------------- Read-side enrichment ----------------------

func winnerStatusFromOffer(a *domain.Auction, o *domain.WinnerOffer) string {
	if o == nil {
		if a.Status == domain.AuctionStatusExecuted && a.WinnerBidID != nil {
			// Drawn before pay-only-if-you-win: the winner paid their bid with the ticket.
			return domain.WinnerPaymentStatusPaid
		}
		return domain.WinnerPaymentStatusNone
	}
	switch o.Status {
	case domain.WinnerOfferPaid:
		return domain.WinnerPaymentStatusPaid
	case domain.WinnerOfferAwaitingPayment:
		return domain.WinnerPaymentStatusAwaiting
	}
	if a.Status == domain.AuctionStatusCancelled {
		return domain.WinnerPaymentStatusUnpaid
	}
	return domain.WinnerPaymentStatusAwaiting // expired and the next offer is being opened
}

// EnrichWinnerPayment fills the public winner-payment fields and hides the winner's identity
// until they have paid (naming someone who then does not pay would mislead other players).
// When viewerID is set it also fills MyResult for that participant.
func (s *Service) EnrichWinnerPayment(a *domain.Auction, viewerID string) {
	s.enrichWinnerPayment(a, viewerID, true)
}

// EnrichWinnerPaymentForStaff fills the winner-payment fields without hiding the winner.
func (s *Service) EnrichWinnerPaymentForStaff(a *domain.Auction) {
	s.enrichWinnerPayment(a, "", false)
}

func (s *Service) enrichWinnerPayment(a *domain.Auction, viewerID string, hideUnpaidWinner bool) {
	if a == nil {
		return
	}
	var latest *domain.WinnerOffer
	if s.winnerOffers != nil && (a.Status == domain.AuctionStatusExecuted || a.Status == domain.AuctionStatusCancelled) {
		latest, _ = s.winnerOffers.GetLatest(a.ID)
	}
	if strings.TrimSpace(viewerID) != "" {
		a.MyResult = s.myResult(a, latest, viewerID)
	}
	s.applyWinnerPayment(a, latest, hideUnpaidWinner)
}

func (s *Service) applyWinnerPayment(a *domain.Auction, latest *domain.WinnerOffer, hideUnpaidWinner bool) {
	if s.winnerOffers == nil {
		return // feature off: keep the old behaviour
	}
	a.WinnerPaymentStatus = winnerStatusFromOffer(a, latest)
	if latest != nil && latest.IsAwaiting() {
		due := latest.DueAt
		a.WinnerPaymentDueAt = &due
	}
	if hideUnpaidWinner && a.WinnerPaymentStatus != domain.WinnerPaymentStatusPaid {
		a.WinnerName = ""
		a.WinnerParticipantID = nil
	}
}

// myResult works out what the viewer should see about their own outcome.
func (s *Service) myResult(a *domain.Auction, latest *domain.WinnerOffer, viewerID string) *domain.MyAuctionResult {
	switch a.Status {
	case domain.AuctionStatusDraft, domain.AuctionStatusActive, domain.AuctionStatusPostponed, domain.AuctionStatusClosed:
		return &domain.MyAuctionResult{Outcome: domain.MyOutcomePending}
	}
	if s.winnerOffers != nil {
		offers, err := s.winnerOffers.ListByAuction(a.ID)
		if err == nil {
			for i := len(offers) - 1; i >= 0; i-- {
				o := offers[i]
				if !strings.EqualFold(o.ParticipantID, viewerID) {
					continue
				}
				bid := o.BidValue
				res := &domain.MyAuctionResult{BidValue: &bid, Attempt: o.Attempt}
				switch o.Status {
				case domain.WinnerOfferPaid:
					res.Outcome = domain.MyOutcomeWonPaid
				case domain.WinnerOfferAwaitingPayment:
					res.Outcome = domain.MyOutcomeWonPayNow
					due := o.DueAt
					res.DueAt = &due
				default:
					res.Outcome = domain.MyOutcomeWonExpired
				}
				return res
			}
		}
	}
	if a.Status == domain.AuctionStatusCancelled {
		return &domain.MyAuctionResult{Outcome: domain.MyOutcomeCancelled}
	}
	if latest == nil && a.WinnerParticipantID != nil && strings.EqualFold(*a.WinnerParticipantID, viewerID) {
		// Drawn before pay-only-if-you-win: the bid was paid with the ticket.
		return &domain.MyAuctionResult{Outcome: domain.MyOutcomeWonPaid, BidValue: a.WinnerBidValue}
	}
	if a.WinnerBidID == nil && latest == nil {
		return &domain.MyAuctionResult{Outcome: domain.MyOutcomeNoWinner}
	}
	return &domain.MyAuctionResult{Outcome: domain.MyOutcomeLost}
}

// winnerPaymentStatuses returns the public winner-payment status per auction for list views.
func (s *Service) winnerPaymentStatuses(auctions []domain.Auction) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	if s.winnerOffers == nil || len(auctions) == 0 {
		return out
	}
	ids := make([]uuid.UUID, 0, len(auctions))
	for i := range auctions {
		if auctions[i].Status == domain.AuctionStatusExecuted || auctions[i].Status == domain.AuctionStatusCancelled {
			ids = append(ids, auctions[i].ID)
		}
	}
	latest, err := s.winnerOffers.LatestByAuctions(ids)
	if err != nil {
		return out
	}
	for i := range auctions {
		a := &auctions[i]
		if o, ok := latest[a.ID]; ok {
			out[a.ID] = winnerStatusFromOffer(a, &o)
		}
	}
	return out
}
