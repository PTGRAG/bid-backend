package chapapayment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
)

var ErrFulfillmentDeferred = errors.New("chapa: participation fulfillment deferred")

type VerifiedSnapshot struct {
	TxRef          string
	ChapaReference string
	APIStatus      string
	DataStatus     string
	Amount         interface{}
	Meta           map[string]interface{}
	CustomerEmail  string
	CustomerFirst  string
	BidValue       domain.Money   // legacy single bid
	BidValues      []domain.Money // pack of bids (preferred)
}

type Service struct {
	repo            ports.PaymentLedgerRepository
	lubSvc          *lub.Service
	users           ports.ParticipantUserRepository
	notificationSvc ports.NotificationService
	chapa           ports.PaymentProvider
	metrics         ports.MetricsProvider
	admin           ports.AdminNotifier
	log             *slog.Logger
	pendingBids     sync.Map // map[string][]domain.Money keyed by txRef
	placedTxBids    sync.Map // map[string]bool keyed by txRef for idempotency
}

func NewService(
	repo ports.PaymentLedgerRepository,
	lubSvc *lub.Service,
	users ports.ParticipantUserRepository,
	notificationSvc ports.NotificationService,
	log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, lubSvc: lubSvc, users: users, notificationSvc: notificationSvc, log: log}
}

func (s *Service) SetMetrics(m ports.MetricsProvider) {
	s.metrics = m
}

func (s *Service) SetAdminNotifier(n ports.AdminNotifier) {
	s.admin = n
}

func (s *Service) SetChapaClient(c ports.PaymentProvider) {
	s.chapa = c
}

func (s *Service) alertAdmin(ctx context.Context, text string) {
	if s.admin == nil {
		return
	}
	s.admin.NotifyAdmin(ctx, text)
}

func (s *Service) capturePaymentIssue(txRef, kind string, err error) {
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("payment.tx_ref", txRef)
		scope.SetTag("payment.issue", kind)
		scope.SetLevel(sentry.LevelError)
		if err != nil {
			sentry.CaptureException(err)
			return
		}
		sentry.CaptureMessage(kind + ": " + txRef)
	})
}

func (s *Service) SetPendingBid(txRef string, bidValue domain.Money) {
	if bidValue.IsPositive() {
		s.SetPendingBids(txRef, []domain.Money{bidValue})
	}
}

func (s *Service) SetPendingBids(txRef string, bidValues []domain.Money) {
	txRef = strings.TrimSpace(txRef)
	if txRef == "" {
		return
	}
	cleaned := make([]domain.Money, 0, len(bidValues))
	for _, v := range bidValues {
		if v.IsPositive() {
			cleaned = append(cleaned, v)
		}
	}
	if len(cleaned) == 0 {
		return
	}
	s.pendingBids.Store(txRef, cleaned)
}

func encodeBidValues(bidValues []domain.Money) string {
	parts := make([]string, 0, len(bidValues))
	for _, v := range bidValues {
		if v.IsPositive() {
			parts = append(parts, v.String())
		}
	}
	return strings.Join(parts, "_")
}

func decodeBidValues(raw string) []domain.Money {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '_' || r == ';' || r == '|'
	})
	out := make([]domain.Money, 0, len(parts))
	for _, part := range parts {
		if m, err := domain.NewMoneyFromString(strings.TrimSpace(part)); err == nil && m.IsPositive() {
			out = append(out, m)
		}
	}
	return out
}

// NormalizeChapaMeta accepts the various shapes Chapa returns for custom meta
// (object, string-keyed map, or JSON string).
func NormalizeChapaMeta(raw interface{}) map[string]interface{} {
	switch m := raw.(type) {
	case map[string]interface{}:
		return m
	case map[string]string:
		out := make(map[string]interface{}, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	case string:
		s := strings.TrimSpace(m)
		if s == "" {
			return nil
		}
		var out map[string]interface{}
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	return nil
}

func parseMoneyFromMeta(raw interface{}) (domain.Money, bool) {
	switch v := raw.(type) {
	case string:
		if m, err := domain.NewMoneyFromString(strings.TrimSpace(v)); err == nil && m.IsPositive() {
			return m, true
		}
	case float64:
		if m := domain.NewMoney(v); m.IsPositive() {
			return m, true
		}
	case int:
		if m := domain.NewMoney(float64(v)); m.IsPositive() {
			return m, true
		}
	}
	return domain.Zero(), false
}

func (s *Service) resolveBidValues(ctx context.Context, txRef string, snap *VerifiedSnapshot) []domain.Money {
	if snap != nil && len(snap.BidValues) > 0 {
		out := make([]domain.Money, 0, len(snap.BidValues))
		for _, v := range snap.BidValues {
			if v.IsPositive() {
				out = append(out, v)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if snap != nil && snap.BidValue.IsPositive() {
		return []domain.Money{snap.BidValue}
	}
	if snap != nil && snap.Meta != nil {
		if rawList, ok := snap.Meta["bid_values"].([]interface{}); ok && len(rawList) > 0 {
			out := make([]domain.Money, 0, len(rawList))
			for _, item := range rawList {
				if m, ok := parseMoneyFromMeta(item); ok {
					out = append(out, m)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
		// JSON numbers sometimes decode as []any of float64; also accept []string or comma-separated string in meta.
		if rawStrs, ok := snap.Meta["bid_values"].([]string); ok && len(rawStrs) > 0 {
			out := make([]domain.Money, 0, len(rawStrs))
			for _, item := range rawStrs {
				if m, err := domain.NewMoneyFromString(strings.TrimSpace(item)); err == nil && m.IsPositive() {
					out = append(out, m)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
		if rawCSV, ok := snap.Meta["bid_values"].(string); ok && strings.TrimSpace(rawCSV) != "" {
			if out := decodeBidValues(rawCSV); len(out) > 0 {
				return out
			}
		}
		if m, ok := parseMoneyFromMeta(snap.Meta["bid_value"]); ok {
			return []domain.Money{m}
		}
	}
	if v, ok := s.pendingBids.Load(txRef); ok {
		switch vals := v.(type) {
		case []domain.Money:
			out := make([]domain.Money, 0, len(vals))
			for _, m := range vals {
				if m.IsPositive() {
					out = append(out, m)
				}
			}
			if len(out) > 0 {
				return out
			}
		case domain.Money:
			if vals.IsPositive() {
				return []domain.Money{vals}
			}
		}
	}
	// Durable fallback: bid pack saved on the payments row at checkout.
	if p, err := s.repo.GetByTxRef(ctx, txRef); err == nil && p != nil {
		if out := decodeBidValues(p.BidValues); len(out) > 0 {
			return out
		}
	}
	return nil
}

func (s *Service) placePendingBidsIfNeeded(ctx context.Context, txRef string, auctionID uuid.UUID, participantID string, snap *VerifiedSnapshot) {
	if _, alreadyPlaced := s.placedTxBids.Load(txRef); alreadyPlaced {
		return
	}
	bids := s.resolveBidValues(ctx, txRef, snap)
	if len(bids) == 0 {
		s.log.Warn("chapa_no_bid_values_to_place", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", participantID)
		return
	}
	placed := 0
	for _, bidVal := range bids {
		if _, err := s.lubSvc.PlaceBid(ctx, auctionID, participantID, bidVal); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				s.log.Info("chapa_pack_bid_duplicate_skipped", "tx_ref", txRef, "bid_value", bidVal.String())
				placed++
				continue
			}
			s.log.Warn("chapa_pack_bid_place_failed", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", participantID, "bid_value", bidVal.String(), "error", err)
			continue
		}
		placed++
		s.log.Info("chapa_pack_bid_placed", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", participantID, "bid_value", bidVal.String())
	}
	if placed > 0 {
		s.placedTxBids.Store(txRef, true)
		s.pendingBids.Delete(txRef)
	}
}

// IsChapaStatusInFlight reports whether a Chapa transaction status means "not finished yet"
// (customer still paying / settling) as opposed to a definite failure.
func IsChapaStatusInFlight(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "created", "processing", "initiated":
		return true
	}
	return false
}

func (s *Service) ProcessVerified(ctx context.Context, snap *VerifiedSnapshot) error {
	txRef := strings.TrimSpace(snap.TxRef)
	if txRef == "" {
		return fmt.Errorf("chapa: empty tx_ref")
	}

	chapaOK := strings.EqualFold(strings.TrimSpace(snap.APIStatus), "success") &&
		strings.EqualFold(strings.TrimSpace(snap.DataStatus), "success")

	// Chapa reports "pending" while the customer is still on the checkout page or the payment is
	// settling (the return URL and webhook can both arrive before it is final). That is not a
	// failure: leave the ledger row pending (the reconciler / a later webhook settles it) and send
	// no "payment failed" message.
	if !chapaOK && strings.EqualFold(strings.TrimSpace(snap.APIStatus), "success") &&
		IsChapaStatusInFlight(snap.DataStatus) {
		s.log.Info("chapa_payment_still_pending", "tx_ref", txRef, "data_status", snap.DataStatus)
		return nil
	}

	// The entrance waiver is decided server-side at checkout and stored on the ledger row.
	// Never derive it from Chapa meta or the request.
	checkoutRow, rowErr := s.repo.GetByTxRef(ctx, txRef)
	if rowErr != nil && !errors.Is(rowErr, domain.ErrNotFound) {
		return fmt.Errorf("payment ledger lookup: %w", rowErr)
	}
	waived := checkoutRow != nil && checkoutRow.EntranceWaived

	auctionID, userID, pricePaid, parseErr := parseParticipationMeta(snap)
	if parseErr != nil {
		existing, lookupErr := s.repo.GetByTxRef(ctx, txRef)
		if lookupErr == nil && existing != nil &&
			existing.UserID != nil && existing.AuctionID != nil {
			auctionID = *existing.AuctionID
			userID = *existing.UserID
			pricePaid = existing.Amount
			parseErr = nil
			s.log.Info("chapa_meta_recovered_from_pending_row", "tx_ref", txRef)
		}
	}
	if parseErr != nil {
		p := &domain.Payment{
			TxRef:     txRef,
			Status:    domain.PaymentStatusFailed,
			Processed: true,
			Amount:    domain.Zero(),
			LastError: domain.FormatErrorDetail(parseErr),
		}
		if err := s.repo.Upsert(ctx, p); err != nil {
			return fmt.Errorf("payment ledger upsert (meta invalid): %w", err)
		}
		s.log.Warn("chapa_payment_meta_invalid", "tx_ref", txRef, "error", parseErr)
		if s.metrics != nil {
			s.metrics.RecordPaymentFailure()
		}
		s.capturePaymentIssue(txRef, "chapa_meta_invalid", parseErr)
		s.notifyBuyerAndAdminFailure(ctx, txRef, "", uuid.Nil, "invalid payment meta: "+domain.FormatErrorDetail(parseErr))
		return nil
	}

	if !chapaOK {
		reason := fmt.Sprintf("chapa verify status not success (api=%q data=%q)", snap.APIStatus, snap.DataStatus)
		if waived {
			// Give the free-entrance slot back in the same step that fails the payment.
			if _, fErr := s.repo.FailPendingWaived(ctx, txRef, reason); fErr != nil {
				return fmt.Errorf("release free entrance reservation: %w", fErr)
			}
		}
		p := &domain.Payment{
			TxRef:     txRef,
			UserID:    ptrUUID(userID),
			AuctionID: ptrUUID(auctionID),
			Amount:    pricePaid,
			Status:    domain.PaymentStatusFailed,
			Processed: true,
			LastError: reason,
		}
		if err := s.repo.Upsert(ctx, p); err != nil {
			return fmt.Errorf("payment ledger upsert (chapa not success): %w", err)
		}
		s.log.Warn("chapa_payment_not_success", "tx_ref", txRef, "api_status", snap.APIStatus, "data_status", snap.DataStatus)
		if s.metrics != nil {
			s.metrics.RecordPaymentFailure()
		}
		s.capturePaymentIssue(txRef, "chapa_not_success", nil)
		s.notifyBuyerAndAdminFailure(ctx, txRef, userID.String(), auctionID, reason)
		return nil
	}

	if waived && checkoutRow.Status == domain.PaymentStatusFailed {
		// The checkout was released (abandoned / superseded / reported not-success) yet the
		// customer did pay. Try to take the slot back; if it is gone, fulfil as a normal
		// participation and flag the entrance-fee shortfall for staff.
		reopened, rErr := s.repo.ReopenWaivedReservation(ctx, txRef)
		if rErr != nil {
			return fmt.Errorf("reopen free entrance reservation: %w", rErr)
		}
		if !reopened {
			waived = false
			s.log.Warn("chapa_waived_reservation_lost", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", userID.String())
			s.capturePaymentIssue(txRef, "waived_reservation_lost", nil)
			s.alertAdmin(ctx, fmt.Sprintf(
				"⚠️ Free-entrance checkout paid after its slot was released\ntx_ref: %s\nauction: %s\nuser: %s\npaid: %s ETB (entrance fee not collected)",
				txRef, auctionID.String(), userID.String(), pricePaid.String(),
			))
		}
	}

	p := &domain.Payment{
		TxRef:          txRef,
		ChapaReference: strings.TrimSpace(snap.ChapaReference),
		UserID:         ptrUUID(userID),
		AuctionID:      ptrUUID(auctionID),
		Amount:         pricePaid,
		Status:         domain.PaymentStatusSuccess,
		Processed:      false,
		LastError:      "",
	}
	if err := s.repo.Upsert(ctx, p); err != nil {
		return fmt.Errorf("payment ledger upsert: %w", err)
	}
	if s.metrics != nil {
		s.metrics.RecordPaymentSuccess(pricePaid.Float64())
	}

	if existingPart, err := s.lubSvc.GetParticipation(auctionID, userID.String()); err == nil && existingPart != nil && existingPart.Status == domain.ParticipationStatusActive {
		s.placePendingBidsIfNeeded(ctx, txRef, auctionID, userID.String(), snap)
		if err := s.repo.MarkProcessed(ctx, txRef); err != nil {
			s.log.Error("chapa_mark_processed_failed", "tx_ref", txRef, "error", err)
		}
		s.log.Info("chapa_fulfillment_idempotent_participation", "tx_ref", txRef)
		return nil
	}

	record := s.lubSvc.RecordParticipation
	if waived {
		record = s.lubSvc.RecordFreeEntranceParticipation
	}
	part, err := record(ctx, auctionID, userID.String(), txRef, pricePaid)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			s.placePendingBidsIfNeeded(ctx, txRef, auctionID, userID.String(), snap)
			if err := s.repo.MarkProcessed(ctx, txRef); err != nil {
				s.log.Error("chapa_mark_processed_after_conflict_failed", "tx_ref", txRef, "error", err)
			}
			s.log.Info("chapa_fulfillment_idempotent_record", "tx_ref", txRef)
			return nil
		}
		_ = s.repo.SetFulfillmentFailure(ctx, txRef, err)
		if domain.IsRetryableFulfillmentError(err) {
			s.log.Error("chapa_participation_record_failed", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", userID.String(), "error", err)
			s.capturePaymentIssue(txRef, "fulfillment_deferred", err)
			s.alertAdmin(ctx, fmt.Sprintf(
				"⏳ Payment success but fulfillment deferred\ntx_ref: %s\nauction: %s\nuser: %s\nerror: %s",
				txRef, auctionID.String(), userID.String(), domain.FormatErrorDetail(err),
			))
			return ErrFulfillmentDeferred
		} else {
			s.log.Warn("chapa_participation_record_failed_no_retry", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", userID.String(), "error", err)
			s.capturePaymentIssue(txRef, "fulfillment_failed", err)
			s.notifyBuyerAndAdminFailure(ctx, txRef, userID.String(), auctionID,
				"payment captured but ticket fulfillment failed: "+domain.FormatErrorDetail(err))
			if mErr := s.repo.MarkProcessed(ctx, txRef); mErr != nil {
				s.log.Error("chapa_mark_processed_failed_after_non_retryable", "tx_ref", txRef, "error", mErr)
			}
			return nil
		}
	}

	if part != nil {
		p.ParticipationID = &part.ID
		_ = s.repo.Upsert(ctx, p)
	}

	s.placePendingBidsIfNeeded(ctx, txRef, auctionID, userID.String(), snap)
	s.notifyBuyerAndAdminSuccess(ctx, txRef, userID.String(), auctionID, pricePaid)
	if s.metrics != nil {
		s.metrics.RecordParticipation(pricePaid.Float64())
	}

	if err := s.repo.MarkProcessed(ctx, txRef); err != nil {
		s.log.Error("chapa_mark_processed_failed", "tx_ref", txRef, "error", err)
		return fmt.Errorf("mark processed: %w", err)
	}
	s.log.Info("chapa_fulfillment_complete", "tx_ref", txRef, "auction_id", auctionID.String(), "user_id", userID.String())
	return nil
}

func (s *Service) auctionTitle(auctionID uuid.UUID) string {
	if a, aErr := s.lubSvc.GetAuctionByID(auctionID); aErr == nil && a != nil {
		return a.Title
	}
	return ""
}

func (s *Service) notifyBuyerAndAdminSuccess(ctx context.Context, txRef, participantID string, auctionID uuid.UUID, feePaid domain.Money) {
	title := s.auctionTitle(auctionID)
	if s.notificationSvc != nil {
		if err := s.notificationSvc.NotifyParticipationConfirmed(ctx, participantID, auctionID, title, feePaid, txRef); err != nil {
			s.log.Error("failed to send participation notification", "tx_ref", txRef, "user_id", participantID, "error", err)
		}
	}
	s.alertAdmin(ctx, fmt.Sprintf(
		"✅ Payment success\ntx_ref: %s\namount: %s ETB\nauction: %s (%s)\nuser: %s",
		txRef, feePaid.String(), title, auctionID.String(), participantID,
	))
}

func (s *Service) notifyBuyerAndAdminFailure(ctx context.Context, txRef, participantID string, auctionID uuid.UUID, reason string) {
	if participantID != "" && auctionID != uuid.Nil && s.notificationSvc != nil {
		if err := s.notificationSvc.NotifyPaymentFailed(ctx, participantID, auctionID, txRef, reason); err != nil {
			s.log.Error("failed to send payment-failed notification", "tx_ref", txRef, "user_id", participantID, "error", err)
		}
	}
	title := ""
	if auctionID != uuid.Nil {
		title = s.auctionTitle(auctionID)
	}
	s.alertAdmin(ctx, fmt.Sprintf(
		"❌ Payment failed\ntx_ref: %s\nauction: %s (%s)\nuser: %s\nreason: %s",
		txRef, title, auctionID.String(), participantID, reason,
	))
}

func (s *Service) tryFulfillPayment(ctx context.Context, p *domain.Payment) bool {
	if p.UserID == nil || p.AuctionID == nil {
		s.log.Warn("chapa_retry_skip_incomplete", "tx_ref", p.TxRef)
		return false
	}
	txRef := p.TxRef
	if existingPart, err := s.lubSvc.GetParticipation(*p.AuctionID, p.UserID.String()); err == nil && existingPart != nil && existingPart.Status == domain.ParticipationStatusActive {
		s.placePendingBidsIfNeeded(ctx, txRef, *p.AuctionID, p.UserID.String(), nil)
		if err := s.repo.MarkProcessed(ctx, txRef); err != nil {
			s.log.Error("chapa_retry_mark_processed_failed", "tx_ref", txRef, "error", err)
			return false
		}
		return true
	}
	record := s.lubSvc.RecordParticipation
	if p.EntranceWaived {
		record = s.lubSvc.RecordFreeEntranceParticipation
	}
	_, err := record(ctx, *p.AuctionID, p.UserID.String(), txRef, p.Amount)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			s.placePendingBidsIfNeeded(ctx, txRef, *p.AuctionID, p.UserID.String(), nil)
			_ = s.repo.MarkProcessed(ctx, txRef)
			return true
		}
		_ = s.repo.SetFulfillmentFailure(ctx, txRef, err)
		if domain.IsRetryableFulfillmentError(err) {
			s.log.Error("chapa_retry_participation_failed", "tx_ref", txRef, "error", err)
			return false
		} else {
			s.log.Warn("chapa_retry_participation_failed_no_retry", "tx_ref", txRef, "error", err)
			_ = s.repo.MarkProcessed(ctx, txRef)
			return true
		}
	}
	s.placePendingBidsIfNeeded(ctx, txRef, *p.AuctionID, p.UserID.String(), nil)
	s.notifyBuyerAndAdminSuccess(ctx, txRef, p.UserID.String(), *p.AuctionID, p.Amount)
	if err := s.repo.MarkProcessed(ctx, txRef); err != nil {
		s.log.Error("chapa_retry_mark_processed_failed", "tx_ref", txRef, "error", err)
		return false
	}
	s.log.Info("chapa_retry_fulfilled", "tx_ref", txRef)
	return true
}

func (s *Service) GetPaymentByTxRef(ctx context.Context, txRef string) (*domain.Payment, error) {
	return s.repo.GetByTxRef(ctx, strings.TrimSpace(txRef))
}

// EnsureBidsForPayment places any missing bid pack for an already-successful payment
// (e.g. return-URL verify after webhook fulfilled participation without in-memory bids).
func (s *Service) EnsureBidsForPayment(ctx context.Context, txRef string, bidValues []domain.Money) {
	txRef = strings.TrimSpace(txRef)
	if txRef == "" {
		return
	}
	if len(bidValues) > 0 {
		s.SetPendingBids(txRef, bidValues)
	}
	p, err := s.repo.GetByTxRef(ctx, txRef)
	if err != nil || p == nil || p.UserID == nil || p.AuctionID == nil {
		return
	}
	snap := &VerifiedSnapshot{TxRef: txRef, BidValues: bidValues}
	s.placePendingBidsIfNeeded(ctx, txRef, *p.AuctionID, p.UserID.String(), snap)
}

func (s *Service) RetryPaymentByTxRef(ctx context.Context, txRef string) (fulfilled bool, err error) {
	txRef = strings.TrimSpace(txRef)
	if txRef == "" {
		return false, nil
	}
	p, err := s.repo.GetByTxRef(ctx, txRef)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p.Status != domain.PaymentStatusSuccess || p.Processed {
		return false, nil
	}
	return s.tryFulfillPayment(ctx, p), nil
}

func (s *Service) CreatePendingPayment(ctx context.Context, txRef string, userID, auctionID uuid.UUID, amount domain.Money, bidValues ...domain.Money) error {
	p := &domain.Payment{
		TxRef:     txRef,
		UserID:    ptrUUID(userID),
		AuctionID: ptrUUID(auctionID),
		Amount:    amount,
		BidValues: encodeBidValues(bidValues),
		Status:    domain.PaymentStatusPending,
		Processed: false,
	}
	if err := s.repo.Upsert(ctx, p); err != nil {
		return fmt.Errorf("chapa: create pending payment: %w", err)
	}
	if len(bidValues) > 0 {
		s.SetPendingBids(txRef, bidValues)
	}
	s.log.Info("chapa_pending_payment_created", "tx_ref", txRef, "user_id", userID, "auction_id", auctionID, "bid_values", p.BidValues)
	return nil
}

const (
	// waivedCheckoutTTL is how long an unpaid free-entrance checkout may hold its slot.
	waivedCheckoutTTL = 2 * time.Hour
	// waivedUnverifiableGrace is how long we wait before treating a checkout Chapa cannot
	// verify at all (never initialised / unknown) as abandoned.
	waivedUnverifiableGrace = 30 * time.Minute
)

// CreateWaivedPendingPayment records a checkout whose entrance fee is waived by the free-entrance
// promo. amount must be the bid total only. The slot reservation and the ledger row are created
// in one transaction; domain.ErrFreeEntranceUnavailable means the slot is no longer available.
func (s *Service) CreateWaivedPendingPayment(ctx context.Context, txRef string, userID, auctionID uuid.UUID, amount domain.Money, bidValues ...domain.Money) error {
	p := &domain.Payment{
		TxRef:     txRef,
		UserID:    ptrUUID(userID),
		AuctionID: ptrUUID(auctionID),
		Amount:    amount,
		BidValues: encodeBidValues(bidValues),
		Status:    domain.PaymentStatusPending,
	}
	if err := s.repo.CreateWaivedPending(ctx, p); err != nil {
		if errors.Is(err, domain.ErrFreeEntranceUnavailable) {
			return err
		}
		return fmt.Errorf("chapa: create waived pending payment: %w", err)
	}
	if len(bidValues) > 0 {
		s.SetPendingBids(txRef, bidValues)
	}
	s.log.Info("chapa_waived_pending_payment_created", "tx_ref", txRef, "user_id", userID, "auction_id", auctionID, "amount", amount.String(), "bid_values", p.BidValues)
	return nil
}

// AbortWaivedCheckout releases the free-entrance slot of a checkout that never reached Chapa
// (e.g. initialisation failed), so the customer and the auction are not left holding it.
func (s *Service) AbortWaivedCheckout(ctx context.Context, txRef, reason string) {
	released, err := s.repo.FailPendingWaived(ctx, txRef, reason)
	if err != nil {
		s.log.Error("chapa_waived_abort_failed", "tx_ref", txRef, "error", err)
		return
	}
	if released {
		s.log.Info("chapa_waived_checkout_aborted", "tx_ref", txRef, "reason", reason)
	}
}

// SettleStaleWaivedCheckouts resolves the customer's earlier in-flight free-entrance checkouts
// before a new one is reserved (one free entrance per customer at a time). Each earlier checkout
// is verified with Chapa: if it was actually paid it is fulfilled; otherwise it is released.
func (s *Service) SettleStaleWaivedCheckouts(ctx context.Context, userID uuid.UUID) error {
	rows, err := s.repo.ListPendingWaivedByUser(ctx, userID)
	if err != nil {
		return err
	}
	for i := range rows {
		row := rows[i]
		if s.chapa == nil {
			return domain.NewAppError(503, "Payment gateway is not available right now. Please try again shortly.", nil)
		}
		snap, vErr := s.chapa.VerifyTransaction(row.TxRef)
		if vErr != nil {
			if time.Since(row.CreatedAt) < waivedUnverifiableGrace {
				return domain.NewAppError(409,
					"You have a free-entrance payment in progress. Finish it, or try again in a few minutes.", vErr)
			}
			s.AbortWaivedCheckout(ctx, row.TxRef, "abandoned: unverifiable at Chapa")
			continue
		}

		status := strings.TrimSpace(snap.Status)
		if strings.EqualFold(status, "success") {
			verified := &VerifiedSnapshot{
				TxRef:          row.TxRef,
				ChapaReference: snap.Reference,
				APIStatus:      "success",
				DataStatus:     status,
				Amount:         snap.Amount,
				Meta:           NormalizeChapaMeta(snap.Meta),
				CustomerEmail:  snap.CustomerEmail,
				CustomerFirst:  snap.CustomerFirst,
				BidValues:      decodeBidValues(row.BidValues),
			}
			if pErr := s.ProcessVerified(ctx, verified); pErr != nil && !errors.Is(pErr, ErrFulfillmentDeferred) {
				return pErr
			}
			continue
		}
		s.AbortWaivedCheckout(ctx, row.TxRef, "superseded by a new checkout")
	}
	return nil
}

func (s *Service) RetryUnprocessedPayments(ctx context.Context, limit int) (int, error) {
	rows, err := s.repo.ListSuccessUnprocessed(ctx, limit)
	if err != nil {
		return 0, err
	}
	done := 0
	for i := range rows {
		if s.tryFulfillPayment(ctx, &rows[i]) {
			done++
		}
	}
	return done, nil
}

// ReconcilePendingPayments verifies stale pending checkouts with Chapa and fulfills
// ones that succeeded when the webhook/return URL never reached us (common in Mini App).
func (s *Service) ReconcilePendingPayments(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if s.chapa == nil {
		return 0, nil
	}
	if olderThan <= 0 {
		olderThan = 2 * time.Minute
	}
	rows, err := s.repo.ListPendingOlderThan(ctx, time.Now().Add(-olderThan), limit)
	if err != nil {
		return 0, err
	}
	done := 0
	for i := range rows {
		txRef := rows[i].TxRef
		snap, vErr := s.chapa.VerifyTransaction(txRef)
		if vErr != nil {
			s.log.Warn("chapa_reconcile_verify_failed", "tx_ref", txRef, "error", vErr)
			// Chapa still can't see this checkout a day later: it was never paid. Free the slot.
			if rows[i].EntranceWaived && time.Since(rows[i].CreatedAt) > 24*time.Hour {
				s.AbortWaivedCheckout(ctx, txRef, "expired: checkout unknown to Chapa")
			}
			continue
		}
		status := strings.TrimSpace(snap.Status)
		if IsChapaStatusInFlight(status) {
			// An unpaid free-entrance checkout must not hold a slot forever.
			if rows[i].EntranceWaived && time.Since(rows[i].CreatedAt) > waivedCheckoutTTL {
				s.AbortWaivedCheckout(ctx, txRef, "expired: unpaid free-entrance checkout")
			}
			continue
		}
		verified := &VerifiedSnapshot{
			TxRef:          txRef,
			ChapaReference: snap.Reference,
			APIStatus:      "success",
			DataStatus:     status,
			Amount:         snap.Amount,
			Meta:           NormalizeChapaMeta(snap.Meta),
			CustomerEmail:  snap.CustomerEmail,
			CustomerFirst:  snap.CustomerFirst,
			BidValues:      decodeBidValues(rows[i].BidValues),
		}
		if err := s.ProcessVerified(ctx, verified); err != nil {
			if errors.Is(err, ErrFulfillmentDeferred) {
				s.log.Info("chapa_reconcile_deferred", "tx_ref", txRef)
				continue
			}
			s.log.Warn("chapa_reconcile_process_failed", "tx_ref", txRef, "error", err)
			continue
		}
		done++
		s.log.Info("chapa_reconcile_fulfilled", "tx_ref", txRef, "chapa_status", status)
	}
	return done, nil
}

func (s *Service) ListSuccessUnprocessed(ctx context.Context, limit int) ([]domain.Payment, error) {
	return s.repo.ListSuccessUnprocessed(ctx, limit)
}

func metaString(meta map[string]interface{}, keys ...string) string {
	if meta == nil {
		return ""
	}
	for _, k := range keys {
		switch v := meta[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case fmt.Stringer:
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

func parseParticipationMeta(snap *VerifiedSnapshot) (auctionID, userID uuid.UUID, price domain.Money, err error) {
	meta := snap.Meta
	if meta == nil {
		return uuid.Nil, uuid.Nil, domain.Zero(), fmt.Errorf("chapa webhook missing meta: %w", domain.ErrInvalidInput)
	}
	as := metaString(meta, "auction_id", "product_id", "auctionId", "productId")
	us := metaString(meta, "user_id", "userId", "participant_id", "participantId")
	auctionID, _ = uuid.Parse(strings.TrimSpace(as))
	userID, _ = uuid.Parse(strings.TrimSpace(us))
	if auctionID == uuid.Nil || userID == uuid.Nil {
		return uuid.Nil, uuid.Nil, domain.Zero(), fmt.Errorf("chapa webhook missing auction_id or user_id: %w", domain.ErrInvalidInput)
	}
	switch a := snap.Amount.(type) {
	case string:
		price, err = domain.NewMoneyFromString(strings.TrimSpace(a))
		if err != nil {
			return uuid.Nil, uuid.Nil, domain.Zero(), err
		}
	case float64:
		price = domain.NewMoney(a)
	default:
		price = domain.Zero()
	}
	return auctionID, userID, price, nil
}

func ptrUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
