package chapapayment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// Pay only if you win: tickets cost the entrance fee only (a single free ticket costs nothing and
// skips Chapa), and the winner pays their bid amount after the draw through a "winner" payment.

// FulfillFreeCheckout completes a free-entrance checkout whose total is 0 (one free ticket): there
// is nothing to charge, so the ticket is issued right away without going through Chapa.
func (s *Service) FulfillFreeCheckout(ctx context.Context, txRef string) error {
	txRef = strings.TrimSpace(txRef)
	row, err := s.repo.GetByTxRef(ctx, txRef)
	if err != nil {
		return err
	}
	if row.UserID == nil || row.AuctionID == nil || !row.EntranceWaived || !row.Amount.IsZero() ||
		row.Purpose == domain.PaymentPurposeWinner {
		return fmt.Errorf("checkout %s is not a free ticket: %w", txRef, domain.ErrInvalidInput)
	}
	if row.Status != domain.PaymentStatusPending && row.Status != domain.PaymentStatusSuccess {
		return domain.ErrFreeEntranceUnavailable
	}
	p := &domain.Payment{
		TxRef:     txRef,
		UserID:    row.UserID,
		AuctionID: row.AuctionID,
		Amount:    domain.Zero(),
		Status:    domain.PaymentStatusSuccess,
		Processed: false,
	}
	if err := s.repo.Upsert(ctx, p); err != nil {
		return fmt.Errorf("free ticket ledger upsert: %w", err)
	}
	return s.fulfillEntry(ctx, p, *row.AuctionID, *row.UserID, domain.Zero(), true, &VerifiedSnapshot{TxRef: txRef})
}

// CreateWinnerPendingPayment records the winner's checkout for their bid amount.
func (s *Service) CreateWinnerPendingPayment(ctx context.Context, txRef string, offer *domain.WinnerOffer) error {
	if offer == nil {
		return fmt.Errorf("winner payment needs an offer: %w", domain.ErrInvalidInput)
	}
	userID, err := uuid.Parse(offer.ParticipantID)
	if err != nil {
		return fmt.Errorf("winner payment participant: %w", domain.ErrInvalidInput)
	}
	offerID := offer.ID
	p := &domain.Payment{
		TxRef:         txRef,
		UserID:        ptrUUID(userID),
		AuctionID:     ptrUUID(offer.AuctionID),
		Amount:        offer.BidValue,
		Purpose:       domain.PaymentPurposeWinner,
		WinnerOfferID: &offerID,
		Status:        domain.PaymentStatusPending,
	}
	if err := s.repo.Upsert(ctx, p); err != nil {
		return fmt.Errorf("chapa: create winner payment: %w", err)
	}
	s.log.Info("chapa_winner_payment_created", "tx_ref", txRef, "offer_id", offerID, "amount", offer.BidValue.String())
	return nil
}

// HasPendingWinnerPayment implements lub.WinnerPaymentChecker: a winner checkout started in the
// last couple of hours may still be completed at Chapa.
func (s *Service) HasPendingWinnerPayment(ctx context.Context, offerID uuid.UUID) (bool, error) {
	return s.repo.HasPendingForOffer(ctx, offerID, time.Now().Add(-abandonedCheckoutTTL))
}

func snapshotAmount(snap *VerifiedSnapshot) (domain.Money, bool) {
	switch a := snap.Amount.(type) {
	case string:
		m, err := domain.NewMoneyFromString(strings.TrimSpace(a))
		return m, err == nil
	case float64:
		return domain.NewMoney(a), true
	}
	return domain.Zero(), false
}

func (s *Service) processWinnerVerified(ctx context.Context, snap *VerifiedSnapshot, row *domain.Payment, chapaOK bool) error {
	txRef := row.TxRef
	auctionID := uuid.Nil
	if row.AuctionID != nil {
		auctionID = *row.AuctionID
	}
	participantID := ""
	if row.UserID != nil {
		participantID = row.UserID.String()
	}
	if row.WinnerOfferID == nil {
		return fmt.Errorf("winner payment %s has no offer", txRef)
	}

	if !chapaOK {
		reason := fmt.Sprintf("chapa verify status not success (api=%q data=%q)", snap.APIStatus, snap.DataStatus)
		failed := &domain.Payment{TxRef: txRef, Status: domain.PaymentStatusFailed, Processed: true, LastError: reason}
		if err := s.repo.Upsert(ctx, failed); err != nil {
			return fmt.Errorf("winner payment upsert (failed): %w", err)
		}
		if s.metrics != nil {
			s.metrics.RecordPaymentFailure()
		}
		s.notifyBuyerAndAdminFailure(ctx, txRef, participantID, auctionID, "winning bid payment: "+reason)
		return nil
	}

	// Chapa must have collected the full bid amount.
	if paid, ok := snapshotAmount(snap); ok && paid.LessThan(row.Amount) {
		msg := fmt.Sprintf("winner paid %s ETB but the bid is %s ETB", paid.String(), row.Amount.String())
		short := &domain.Payment{TxRef: txRef, Status: domain.PaymentStatusSuccess, Processed: true, RetryDisabled: true, LastError: msg}
		if err := s.repo.Upsert(ctx, short); err != nil {
			return fmt.Errorf("winner payment upsert (short): %w", err)
		}
		s.capturePaymentIssue(txRef, "winner_payment_short", nil)
		s.alertAdmin(ctx, fmt.Sprintf("⚠️ Winner payment amount too low. Not accepted.\ntx_ref: %s\nauction: %s\n%s", txRef, auctionID, msg))
		return nil
	}

	ok := &domain.Payment{
		TxRef:          txRef,
		ChapaReference: strings.TrimSpace(snap.ChapaReference),
		Status:         domain.PaymentStatusSuccess,
		Processed:      false,
	}
	if err := s.repo.Upsert(ctx, ok); err != nil {
		return fmt.Errorf("winner payment upsert: %w", err)
	}
	if s.metrics != nil {
		s.metrics.RecordPaymentSuccess(row.Amount.Float64())
	}
	row.Status = domain.PaymentStatusSuccess
	if !s.fulfillWinner(ctx, row) {
		return ErrFulfillmentDeferred
	}
	return nil
}

// fulfillWinner marks the offer paid for a successful winner payment. false = retry later.
func (s *Service) fulfillWinner(ctx context.Context, p *domain.Payment) bool {
	if p.WinnerOfferID == nil {
		s.log.Warn("chapa_winner_payment_without_offer", "tx_ref", p.TxRef)
		return false
	}
	if _, err := s.lubSvc.MarkWinnerOfferPaid(ctx, *p.WinnerOfferID, p.TxRef, p.Amount); err != nil {
		_ = s.repo.SetFulfillmentFailure(ctx, p.TxRef, err)
		if errors.Is(err, domain.ErrNotFound) {
			_ = s.repo.MarkProcessed(ctx, p.TxRef)
			return true
		}
		s.log.Error("chapa_winner_fulfill_failed", "tx_ref", p.TxRef, "error", err)
		return false
	}
	if err := s.repo.MarkProcessed(ctx, p.TxRef); err != nil {
		s.log.Error("chapa_winner_mark_processed_failed", "tx_ref", p.TxRef, "error", err)
		return false
	}
	s.log.Info("chapa_winner_payment_fulfilled", "tx_ref", p.TxRef)
	return true
}
