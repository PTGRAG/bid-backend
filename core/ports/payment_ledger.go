package ports

import (
	"context"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// PaymentListFilter filters staff payment ledger queries.
type PaymentListFilter struct {
	Status        string // empty = all
	Processed     string // "", "true", "false"
	RetryDisabled string // "", "true", "false"
	Search        string
	Limit         int
	Offset        int
}

// PaymentLedgerRepository persists Chapa payment attempts for fulfillment and retries.
type PaymentLedgerRepository interface {
	Upsert(ctx context.Context, p *domain.Payment) error
	GetByTxRef(ctx context.Context, txRef string) (*domain.Payment, error)
	ListSuccessUnprocessed(ctx context.Context, limit int) ([]domain.Payment, error)
	// ListPendingOlderThan returns checkout rows still pending after olderThan (webhook/return missed).
	ListPendingOlderThan(ctx context.Context, olderThan time.Time, limit int) ([]domain.Payment, error)
	ListStaff(ctx context.Context, f PaymentListFilter) ([]domain.PaymentStaffView, int64, error)
	MarkProcessed(ctx context.Context, txRef string) error
	// MarkRefunded flips the payment to refunded and, for free-entrance payments,
	// returns the reserved slot to the auction in the same transaction.
	MarkRefunded(ctx context.Context, txRef string) error

	// CreateWaivedPending atomically (one transaction) reserves a free-entrance slot on the
	// auction and inserts the pending payment row flagged entrance_waived. It returns
	// domain.ErrFreeEntranceUnavailable when the promo is off, the slots are gone, or the
	// customer already holds / used a free entrance.
	CreateWaivedPending(ctx context.Context, p *domain.Payment) error
	// FailPendingWaived marks a still-pending waived payment failed and releases its slot.
	// released=false means the row was not (or no longer) a pending waived reservation.
	FailPendingWaived(ctx context.Context, txRef string, reason string) (released bool, err error)
	// ReopenWaivedReservation re-reserves a slot for a waived payment that was released
	// (failed) but later turned out to be paid. ok=false means the slot could not be re-taken;
	// the waiver flag is cleared so counters stay consistent.
	ReopenWaivedReservation(ctx context.Context, txRef string) (ok bool, err error)
	// ListPendingWaivedByUser returns the customer's in-flight free-entrance checkouts.
	ListPendingWaivedByUser(ctx context.Context, userID uuid.UUID) ([]domain.Payment, error)
	SetChapaReference(ctx context.Context, txRef string, chapaReference string) error
	SetLastError(ctx context.Context, txRef string, msg string) error
	// SetFulfillmentFailure stores the error and disables automatic retry when the failure is final.
	SetFulfillmentFailure(ctx context.Context, txRef string, err error) error

	// Analytics
	SumAmountByStatus(ctx context.Context, status string) (domain.Money, error)
	CountByStatus(ctx context.Context, status string) (int64, error)
	CountSuccessUnprocessed(ctx context.Context) (int64, error)
	GetRevenueByMonth(ctx context.Context, lastMonths int) ([]domain.MonthlyAmount, error)
	GetCountByMonth(ctx context.Context, status string, lastMonths int) ([]domain.MonthlyCount, error)
}

type RefundRecordRepository interface {
	Create(r *domain.RefundRecord) error
	GetByID(id interface{}) (*domain.RefundRecord, error)
	GetByTxRef(txRef string) (*domain.RefundRecord, error)
	UpdateStatus(id interface{}, status, chapaRef, errMsg string) error
	ListAll(search string, limit, offset int) ([]domain.RefundRecord, int64, error)
}
