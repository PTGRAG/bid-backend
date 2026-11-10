package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// Free-entrance reservations live in the payments ledger:
//
//   - a waived checkout = one payments row (entrance_waived=true) + one claimed slot on the
//     auction (auctions.free_entrance_claimed), created together in a single transaction;
//   - the partial unique index ux_payments_one_waived_per_user guarantees a customer holds
//     at most one pending/successful waived checkout at any time (one free auction ever);
//   - the slot is returned in the same transaction that fails or refunds the payment.
//
// Because slot count and ledger row always move together, the counter cannot drift.

const (
	waivedPerUserIndex = "ux_payments_one_waived_per_user"
	// maxFailReasonLen keeps last_error within the TEXT cap used elsewhere.
	maxFailReasonLen = 500
)

var errReopenDenied = errors.New("waived reservation could not be re-taken")

func pgUniqueViolation(err error) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

func releaseAuctionSlot(tx *gorm.DB, auctionID uuid.UUID) error {
	return tx.Exec(`
		UPDATE auctions
		SET free_entrance_claimed = GREATEST(free_entrance_claimed - 1, 0),
		    updated_at = NOW()
		WHERE id = ?`, auctionID).Error
}

// CreateWaivedPending reserves a free slot and records the pending payment atomically.
func (r *PaymentLedgerRepo) CreateWaivedPending(ctx context.Context, p *domain.Payment) error {
	if p == nil || p.UserID == nil || p.AuctionID == nil || strings.TrimSpace(p.TxRef) == "" {
		return fmt.Errorf("waived payment needs tx_ref, user and auction: %w", domain.ErrInvalidInput)
	}
	if !p.Amount.IsPositive() {
		return fmt.Errorf("waived payment amount must be positive: %w", domain.ErrInvalidInput)
	}
	p.EntranceWaived = true
	p.Status = domain.PaymentStatusPending
	p.Processed = false
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Conditional UPDATE is the single source of truth for the cap: it takes the row
		// lock, so concurrent checkouts serialize and can never exceed the limit.
		res := tx.Exec(`
			UPDATE auctions
			SET free_entrance_claimed = free_entrance_claimed + 1,
			    updated_at = NOW()
			WHERE id = ?
			  AND free_entrance_enabled = true
			  AND status IN ('active', 'postponed')
			  AND (free_entrance_limit = 0 OR free_entrance_claimed < free_entrance_limit)`,
			*p.AuctionID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return domain.ErrFreeEntranceUnavailable
		}

		now := time.Now()
		p.CreatedAt = now
		p.UpdatedAt = now
		if err := tx.Create(p).Error; err != nil {
			if constraint, ok := pgUniqueViolation(err); ok && constraint == waivedPerUserIndex {
				return domain.ErrFreeEntranceUnavailable
			}
			return err
		}
		return nil
	})
}

// FailPendingWaived fails a pending waived payment and gives its slot back.
func (r *PaymentLedgerRepo) FailPendingWaived(ctx context.Context, txRef string, reason string) (bool, error) {
	txRef = strings.TrimSpace(txRef)
	if txRef == "" {
		return false, nil
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > maxFailReasonLen {
		reason = reason[:maxFailReasonLen]
	}

	released := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row struct {
			AuctionID *uuid.UUID `gorm:"column:auction_id"`
		}
		res := tx.Raw(`
			UPDATE payments
			SET status = 'failed', processed = true, last_error = ?, updated_at = NOW()
			WHERE tx_ref = ? AND entrance_waived = true AND status = 'pending'
			RETURNING auction_id`, reason, txRef).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		released = true
		if row.AuctionID == nil {
			return nil
		}
		return releaseAuctionSlot(tx, *row.AuctionID)
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// ReopenWaivedReservation re-takes a slot for a released waived payment that was paid after all.
func (r *PaymentLedgerRepo) ReopenWaivedReservation(ctx context.Context, txRef string) (bool, error) {
	txRef = strings.TrimSpace(txRef)
	if txRef == "" {
		return false, nil
	}

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row struct {
			AuctionID *uuid.UUID `gorm:"column:auction_id"`
		}
		res := tx.Raw(`
			SELECT auction_id FROM payments
			WHERE tx_ref = ? AND entrance_waived = true AND status = 'failed'
			FOR UPDATE`, txRef).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 || row.AuctionID == nil {
			return errReopenDenied
		}

		// The customer paid in good faith against a quote that was valid at checkout, so the
		// promo being switched off since then does not matter - only the cap does.
		claim := tx.Exec(`
			UPDATE auctions
			SET free_entrance_claimed = free_entrance_claimed + 1,
			    updated_at = NOW()
			WHERE id = ?
			  AND (free_entrance_limit = 0 OR free_entrance_claimed < free_entrance_limit)`,
			*row.AuctionID)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return errReopenDenied
		}

		if err := tx.Exec(`
			UPDATE payments
			SET status = 'pending', processed = false, last_error = '', updated_at = NOW()
			WHERE tx_ref = ? AND status = 'failed'`, txRef).Error; err != nil {
			if constraint, ok := pgUniqueViolation(err); ok && constraint == waivedPerUserIndex {
				return errReopenDenied
			}
			return err
		}
		return nil
	})
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errReopenDenied) {
		return false, err
	}

	// Could not re-take the slot: drop the waiver so ledger and counter stay consistent.
	if clearErr := r.db.WithContext(ctx).Exec(`
		UPDATE payments
		SET entrance_waived = false, updated_at = NOW()
		WHERE tx_ref = ? AND entrance_waived = true AND status = 'failed'`, txRef).Error; clearErr != nil {
		return false, clearErr
	}
	return false, nil
}

// ListPendingWaivedByUser returns the customer's in-flight free-entrance checkouts (oldest first).
func (r *PaymentLedgerRepo) ListPendingWaivedByUser(ctx context.Context, userID uuid.UUID) ([]domain.Payment, error) {
	var rows []domain.Payment
	err := r.db.WithContext(ctx).
		Select(safePaymentSelect).
		Where("user_id = ? AND entrance_waived = true AND status = ?", userID, domain.PaymentStatusPending).
		Order("created_at ASC").
		Find(&rows).Error
	return rows, err
}
