package postgres

import (
	"errors"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// WinnerOfferRepo implements ports.WinnerOfferRepository.
type WinnerOfferRepo struct {
	db *gorm.DB
}

func NewWinnerOfferRepository(db *gorm.DB) *WinnerOfferRepo {
	return &WinnerOfferRepo{db: db}
}

func (r *WinnerOfferRepo) Create(o *domain.WinnerOffer) error {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.Status == "" {
		o.Status = domain.WinnerOfferAwaitingPayment
	}
	if err := r.db.Create(o).Error; err != nil {
		if _, ok := pgUniqueViolation(err); ok {
			return domain.ErrConflict
		}
		return err
	}
	return nil
}

func (r *WinnerOfferRepo) first(q *gorm.DB) (*domain.WinnerOffer, error) {
	var o domain.WinnerOffer
	if err := q.First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

func (r *WinnerOfferRepo) GetByID(id uuid.UUID) (*domain.WinnerOffer, error) {
	o, err := r.first(r.db.Where("id = ?", id))
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, domain.ErrNotFound
	}
	return o, nil
}

func (r *WinnerOfferRepo) GetOpen(auctionID uuid.UUID) (*domain.WinnerOffer, error) {
	return r.first(r.db.Where("auction_id = ? AND status = ?", auctionID, domain.WinnerOfferAwaitingPayment))
}

func (r *WinnerOfferRepo) GetLatest(auctionID uuid.UUID) (*domain.WinnerOffer, error) {
	return r.first(r.db.Where("auction_id = ?", auctionID).Order("attempt DESC"))
}

func (r *WinnerOfferRepo) ListByAuction(auctionID uuid.UUID) ([]domain.WinnerOffer, error) {
	var out []domain.WinnerOffer
	err := r.db.Where("auction_id = ?", auctionID).Order("attempt ASC").Find(&out).Error
	return out, err
}

func (r *WinnerOfferRepo) LatestByAuctions(auctionIDs []uuid.UUID) (map[uuid.UUID]domain.WinnerOffer, error) {
	out := make(map[uuid.UUID]domain.WinnerOffer, len(auctionIDs))
	if len(auctionIDs) == 0 {
		return out, nil
	}
	var rows []domain.WinnerOffer
	err := r.db.Raw(`
		SELECT DISTINCT ON (auction_id) *
		FROM auction_winner_offers
		WHERE auction_id IN ?
		ORDER BY auction_id, attempt DESC`, auctionIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, o := range rows {
		out[o.AuctionID] = o
	}
	return out, nil
}

func (r *WinnerOfferRepo) ListOpen(limit int) ([]domain.WinnerOffer, error) {
	if limit <= 0 {
		limit = 200
	}
	var out []domain.WinnerOffer
	err := r.db.Where("status = ?", domain.WinnerOfferAwaitingPayment).
		Order("due_at ASC").Limit(limit).Find(&out).Error
	return out, err
}

func (r *WinnerOfferRepo) MarkPaid(id uuid.UUID, txRef string, paidAt time.Time) (bool, error) {
	res := r.db.Model(&domain.WinnerOffer{}).
		Where("id = ? AND status = ?", id, domain.WinnerOfferAwaitingPayment).
		Updates(map[string]interface{}{
			"status":         domain.WinnerOfferPaid,
			"paid_at":        paidAt,
			"payment_tx_ref": txRef,
			"updated_at":     time.Now(),
		})
	return res.RowsAffected == 1, res.Error
}

func (r *WinnerOfferRepo) MarkExpired(id uuid.UUID) (bool, error) {
	res := r.db.Model(&domain.WinnerOffer{}).
		Where("id = ? AND status = ?", id, domain.WinnerOfferAwaitingPayment).
		Updates(map[string]interface{}{
			"status":     domain.WinnerOfferExpired,
			"updated_at": time.Now(),
		})
	return res.RowsAffected == 1, res.Error
}

func (r *WinnerOfferRepo) AdvanceReminder(id uuid.UUID, from int) (bool, error) {
	res := r.db.Model(&domain.WinnerOffer{}).
		Where("id = ? AND status = ? AND reminders_sent = ?", id, domain.WinnerOfferAwaitingPayment, from).
		Updates(map[string]interface{}{
			"reminders_sent": from + 1,
			"updated_at":     time.Now(),
		})
	return res.RowsAffected == 1, res.Error
}
