package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type PaymentLedgerRepo struct {
	db *gorm.DB
}

func NewPaymentLedgerRepository(db *gorm.DB) *PaymentLedgerRepo {
	return &PaymentLedgerRepo{db: db}
}

const safePaymentSelect = `id, tx_ref, chapa_reference, user_id, auction_id, participation_id,
	amount, bid_values, entrance_waived, status, processed, retry_disabled, last_error, created_at, updated_at`

const paymentStaffEnrichSelect = safePaymentSelect + `,
	participant_users.full_name AS participant_name,
	participant_users.email AS participant_email,
	auctions.title AS auction_title,
	EXISTS (
		SELECT 1 FROM auction_participations ap WHERE ap.transaction_id = payments.tx_ref
	) AS has_participation,
	COALESCE((
		SELECT rr.status FROM refund_records rr
		WHERE rr.tx_ref = payments.tx_ref
		ORDER BY rr.created_at DESC
		LIMIT 1
	), '') AS refund_status`

var paymentListSession = &gorm.Session{PrepareStmt: false}

func (r *PaymentLedgerRepo) Upsert(ctx context.Context, p *domain.Payment) error {
	db := r.db.WithContext(ctx)
	var existing domain.Payment
	err := db.Select(safePaymentSelect).Where("tx_ref = ?", p.TxRef).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if p.ID == uuid.Nil {
			p.ID = uuid.New()
		}
		now := time.Now()
		if p.CreatedAt.IsZero() {
			p.CreatedAt = now
		}
		p.UpdatedAt = now
		return db.Create(p).Error
	}
	if err != nil {
		return err
	}
	p.ID = existing.ID
	p.CreatedAt = existing.CreatedAt

	// Never wipe identity / bid pack / amount written at checkout time.
	if p.UserID == nil {
		p.UserID = existing.UserID
	}
	if p.AuctionID == nil {
		p.AuctionID = existing.AuctionID
	}
	if p.ParticipationID == nil {
		p.ParticipationID = existing.ParticipationID
	}
	if p.Amount.IsZero() && !existing.Amount.IsZero() {
		p.Amount = existing.Amount
	}
	if strings.TrimSpace(p.BidValues) == "" {
		p.BidValues = existing.BidValues
	}
	if strings.TrimSpace(p.ChapaReference) == "" {
		p.ChapaReference = existing.ChapaReference
	}

	updates := map[string]interface{}{
		"chapa_reference":  p.ChapaReference,
		"user_id":          p.UserID,
		"auction_id":       p.AuctionID,
		"participation_id": p.ParticipationID,
		"amount":           p.Amount,
		"bid_values":       p.BidValues,
		"status":           p.Status,
		"processed":        p.Processed,
		"retry_disabled":   p.RetryDisabled,
		"last_error":       p.LastError,
		"updated_at":       time.Now(),
	}
	return db.Model(&domain.Payment{}).Where("tx_ref = ?", p.TxRef).Updates(updates).Error
}

func (r *PaymentLedgerRepo) GetByTxRef(ctx context.Context, txRef string) (*domain.Payment, error) {
	var row domain.Payment
	if err := r.db.WithContext(ctx).Select(safePaymentSelect).Where("tx_ref = ?", txRef).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &row, nil
}

func applyPaymentStaffFilters(q *gorm.DB, f ports.PaymentListFilter) *gorm.DB {
	if s := strings.TrimSpace(f.Status); s != "" && s != "all" {
		q = q.Where("payments.status = ?", s)
	}
	switch strings.TrimSpace(f.Processed) {
	case "true":
		q = q.Where("payments.processed = ?", true)
	case "false":
		q = q.Where("payments.processed = ?", false)
	}
	switch strings.TrimSpace(f.RetryDisabled) {
	case "true":
		q = q.Where("payments.retry_disabled = ?", true)
	case "false":
		q = q.Where("payments.retry_disabled = ?", false)
	}
	return q
}

func (r *PaymentLedgerRepo) ListStaff(ctx context.Context, f ports.PaymentListFilter) ([]domain.PaymentStaffView, int64, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	type staffRow struct {
		domain.Payment
		ParticipantName  string `gorm:"column:participant_name"`
		ParticipantEmail string `gorm:"column:participant_email"`
		AuctionTitle     string `gorm:"column:auction_title"`
		HasParticipation bool   `gorm:"column:has_participation"`
		RefundStatus     string `gorm:"column:refund_status"`
	}

	search := strings.TrimSpace(f.Search)

	countQ := applyPaymentStaffFilters(
		r.db.WithContext(ctx).Session(paymentListSession).Table("payments"),
		f,
	)
	if search != "" {
		term := "%" + search + "%"
		countQ = countQ.
			Joins("LEFT JOIN participant_users ON participant_users.id = payments.user_id").
			Joins("LEFT JOIN auctions ON auctions.id = payments.auction_id").
			Where(
				"payments.tx_ref ILIKE ? OR participant_users.email ILIKE ? OR participant_users.full_name ILIKE ? OR auctions.title ILIKE ?",
				term, term, term, term,
			).
			Distinct("payments.id")
	}

	var total int64
	if err := countQ.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	listQ := applyPaymentStaffFilters(
		r.db.WithContext(ctx).Session(paymentListSession).Table("payments").
			Select(paymentStaffEnrichSelect).
			Joins("LEFT JOIN participant_users ON participant_users.id = payments.user_id").
			Joins("LEFT JOIN auctions ON auctions.id = payments.auction_id"),
		f,
	)
	if search != "" {
		term := "%" + search + "%"
		listQ = listQ.Where(
			"payments.tx_ref ILIKE ? OR participant_users.email ILIKE ? OR participant_users.full_name ILIKE ? OR auctions.title ILIKE ?",
			term, term, term, term,
		)
	}

	var rows []staffRow
	err := listQ.Order("payments.updated_at DESC").Limit(limit).Offset(offset).Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	out := make([]domain.PaymentStaffView, len(rows))
	for i := range rows {
		out[i] = domain.PaymentStaffView{
			Payment:          rows[i].Payment,
			ParticipantName:  rows[i].ParticipantName,
			ParticipantEmail: rows[i].ParticipantEmail,
			AuctionTitle:     rows[i].AuctionTitle,
			HasParticipation: rows[i].HasParticipation,
			RefundStatus:     rows[i].RefundStatus,
		}
	}
	return out, total, nil
}

func (r *PaymentLedgerRepo) ListSuccessUnprocessed(ctx context.Context, limit int) ([]domain.Payment, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []domain.Payment
	err := r.db.WithContext(ctx).
		Select(safePaymentSelect).
		Where("status = ? AND processed = ? AND retry_disabled = ?",
			domain.PaymentStatusSuccess, false, false).
		Order("updated_at ASC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

func (r *PaymentLedgerRepo) ListPendingOlderThan(ctx context.Context, olderThan time.Time, limit int) ([]domain.Payment, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []domain.Payment
	err := r.db.WithContext(ctx).
		Select(safePaymentSelect).
		Where("status = ? AND created_at <= ?", domain.PaymentStatusPending, olderThan).
		Order("created_at ASC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

func (r *PaymentLedgerRepo) MarkProcessed(ctx context.Context, txRef string) error {
	return r.db.WithContext(ctx).Model(&domain.Payment{}).
		Where("tx_ref = ?", txRef).
		Updates(map[string]interface{}{
			"processed":  true,
			"last_error": "",
			"updated_at": time.Now(),
		}).Error
}

func (r *PaymentLedgerRepo) SetChapaReference(ctx context.Context, txRef string, chapaReference string) error {
	chapaReference = strings.TrimSpace(chapaReference)
	if chapaReference == "" {
		return nil
	}
	return r.db.WithContext(ctx).Model(&domain.Payment{}).
		Where("tx_ref = ?", txRef).
		Updates(map[string]interface{}{
			"chapa_reference": chapaReference,
			"updated_at":      time.Now(),
		}).Error
}

func (r *PaymentLedgerRepo) MarkRefunded(ctx context.Context, txRef string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var prev struct {
			AuctionID      *uuid.UUID `gorm:"column:auction_id"`
			EntranceWaived bool       `gorm:"column:entrance_waived"`
			Status         string     `gorm:"column:status"`
		}
		if err := tx.Raw(`
			SELECT auction_id, entrance_waived, status FROM payments
			WHERE tx_ref = ? FOR UPDATE`, txRef).Scan(&prev).Error; err != nil {
			return err
		}

		if err := tx.Model(&domain.Payment{}).
			Where("tx_ref = ?", txRef).
			Updates(map[string]interface{}{
				"status":         domain.PaymentStatusRefunded,
				"processed":      true,
				"retry_disabled": true,
				"last_error":     "",
				"updated_at":     time.Now(),
			}).Error; err != nil {
			return err
		}

		// A refunded free-entrance payment no longer holds a slot (and the customer may use
		// their one free entrance again, since the refunded row leaves the unique index).
		holdsSlot := prev.Status == domain.PaymentStatusPending || prev.Status == domain.PaymentStatusSuccess
		if prev.EntranceWaived && holdsSlot && prev.AuctionID != nil {
			return releaseAuctionSlot(tx, *prev.AuctionID)
		}
		return nil
	})
}

func (r *PaymentLedgerRepo) SetLastError(ctx context.Context, txRef string, msg string) error {
	return r.setLastError(ctx, txRef, msg, nil)
}

func (r *PaymentLedgerRepo) SetFulfillmentFailure(ctx context.Context, txRef string, err error) error {
	msg := domain.FormatErrorDetail(err)
	retryDisabled := !domain.IsRetryableFulfillmentError(err)
	return r.setLastError(ctx, txRef, msg, &retryDisabled)
}

func (r *PaymentLedgerRepo) setLastError(ctx context.Context, txRef string, msg string, retryDisabled *bool) error {
	if len(msg) > domain.MaxPaymentLedgerErrorLen {
		msg = msg[:domain.MaxPaymentLedgerErrorLen]
	}
	updates := map[string]interface{}{
		"last_error": msg,
		"updated_at": time.Now(),
	}
	if retryDisabled != nil {
		updates["retry_disabled"] = *retryDisabled
	}
	return r.db.WithContext(ctx).Model(&domain.Payment{}).
		Where("tx_ref = ?", txRef).
		Updates(updates).Error
}

func (r *PaymentLedgerRepo) SumAmountByStatus(ctx context.Context, status string) (domain.Money, error) {
	var sum domain.Money
	err := r.db.WithContext(ctx).Model(&domain.Payment{}).
		Select("COALESCE(SUM(amount), 0)").
		Where("status = ?", status).
		Scan(&sum).Error
	return sum, err
}

func (r *PaymentLedgerRepo) CountByStatus(ctx context.Context, status string) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&domain.Payment{}).
		Where("status = ?", status).
		Count(&n).Error
	return n, err
}

func (r *PaymentLedgerRepo) CountSuccessUnprocessed(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&domain.Payment{}).
		Where("status = ? AND processed = ? AND retry_disabled = ?",
			domain.PaymentStatusSuccess, false, false).
		Count(&n).Error
	return n, err
}

func (r *PaymentLedgerRepo) GetRevenueByMonth(ctx context.Context, lastMonths int) ([]domain.MonthlyAmount, error) {
	if lastMonths <= 0 {
		lastMonths = 12
	}
	cutoff := time.Now().AddDate(0, -lastMonths, 0)
	var result []domain.MonthlyAmount
	err := r.db.WithContext(ctx).Model(&domain.Payment{}).
		Select("to_char(created_at, 'YYYY-MM') as month, COALESCE(SUM(amount), 0) as amount").
		Where("status = ? AND created_at >= ?", domain.PaymentStatusSuccess, cutoff).
		Group("to_char(created_at, 'YYYY-MM')").
		Order("month").
		Scan(&result).Error
	return result, err
}

func (r *PaymentLedgerRepo) GetCountByMonth(ctx context.Context, status string, lastMonths int) ([]domain.MonthlyCount, error) {
	if lastMonths <= 0 {
		lastMonths = 12
	}
	cutoff := time.Now().AddDate(0, -lastMonths, 0)
	q := r.db.WithContext(ctx).Model(&domain.Payment{}).
		Select("to_char(created_at, 'YYYY-MM') as month, count(*) as count").
		Where("created_at >= ?", cutoff)
	if strings.TrimSpace(status) != "" {
		q = q.Where("status = ?", status)
	}
	var result []domain.MonthlyCount
	err := q.Group("to_char(created_at, 'YYYY-MM')").Order("month").Scan(&result).Error
	return result, err
}
