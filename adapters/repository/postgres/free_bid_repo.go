package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FreeBidRepo stores near-miss free bids and their settings.
//
// The balance is never stored: it is the number of grants minus the free bids held by the
// customer's pending or successful checkouts (payments.free_bids_used). A failed or refunded
// checkout therefore gives its free bids back on its own.
type FreeBidRepo struct {
	db *gorm.DB
}

func NewFreeBidRepository(db *gorm.DB) *FreeBidRepo {
	return &FreeBidRepo{db: db}
}

// GetSettings returns the near-miss rules, or the defaults when staff never saved any.
func (r *FreeBidRepo) GetSettings(ctx context.Context) (domain.NearMissSettings, error) {
	var s domain.NearMissSettings
	err := r.db.WithContext(ctx).Where("id = 1").First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.DefaultNearMissSettings(), nil
	}
	return s, err
}

// SaveSettings stores the near-miss rules.
func (r *FreeBidRepo) SaveSettings(ctx context.Context, s domain.NearMissSettings) error {
	s.ID = 1
	s.UpdatedAt = time.Now()
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "runner_ups", "include_duplicates", "max_duplicates", "updated_at", "updated_by"}),
	}).Create(&s).Error
}

// CreateGrants inserts grants, skipping any customer already credited for the same auction.
// It returns the grants that were actually created.
func (r *FreeBidRepo) CreateGrants(ctx context.Context, grants []domain.FreeBidGrant) ([]domain.FreeBidGrant, error) {
	created := make([]domain.FreeBidGrant, 0, len(grants))
	for i := range grants {
		g := grants[i]
		if g.ID == uuid.Nil {
			g.ID = uuid.New()
		}
		if g.CreatedAt.IsZero() {
			g.CreatedAt = time.Now()
		}
		res := r.db.WithContext(ctx).Exec(`
			INSERT INTO free_bid_grants (id, participant_id, auction_id, reason, rank, bid_value, note, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (participant_id, auction_id) WHERE auction_id IS NOT NULL DO NOTHING`,
			g.ID, g.ParticipantID, g.AuctionID, g.Reason, g.Rank, g.BidValue, g.Note, g.CreatedBy, g.CreatedAt)
		if res.Error != nil {
			return created, res.Error
		}
		if res.RowsAffected == 1 {
			created = append(created, g)
		}
	}
	return created, nil
}

// Balance returns a customer's free bids: earned, held by checkouts, and available.
func (r *FreeBidRepo) Balance(ctx context.Context, participantID string) (available, earned, used int, err error) {
	return freeBidBalance(r.db.WithContext(ctx), participantID)
}

func freeBidBalance(db *gorm.DB, participantID string) (available, earned, used int, err error) {
	var row struct {
		Earned int
		Used   int
	}
	err = db.Raw(`
		SELECT
		  (SELECT COUNT(*) FROM free_bid_grants WHERE participant_id = ?) AS earned,
		  (SELECT COALESCE(SUM(free_bids_used), 0) FROM payments
		    WHERE user_id = ? AND free_bids_used > 0 AND status IN ('pending', 'success')) AS used`,
		participantID, participantID).Scan(&row).Error
	if err != nil {
		return 0, 0, 0, err
	}
	available = row.Earned - row.Used
	if available < 0 {
		available = 0
	}
	return available, row.Earned, row.Used, nil
}

// lockFreeBids serialises free-bid spends of one customer for the rest of tx.
func lockFreeBids(tx *gorm.DB, participantID string) error {
	return tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('free_bids:' || ?))`, strings.ToLower(participantID)).Error
}

// reserveFreeBids checks, inside tx and under the customer's lock, that n free bids are still
// available. The caller then writes the payments row with free_bids_used = n in the same tx.
func reserveFreeBids(tx *gorm.DB, participantID string, n int) error {
	if n <= 0 {
		return nil
	}
	if err := lockFreeBids(tx, participantID); err != nil {
		return err
	}
	available, _, _, err := freeBidBalance(tx, participantID)
	if err != nil {
		return err
	}
	if available < n {
		return domain.ErrFreeBidsUnavailable
	}
	return nil
}

// ListByParticipant returns a customer's most recent grants with auction titles.
func (r *FreeBidRepo) ListByParticipant(ctx context.Context, participantID string, limit int) ([]domain.FreeBidGrant, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var rows []domain.FreeBidGrant
	err := r.db.WithContext(ctx).
		Table("free_bid_grants g").
		Select("g.*, COALESCE(a.title, '') AS auction_title").
		Joins("LEFT JOIN auctions a ON a.id = g.auction_id").
		Where("g.participant_id = ?", participantID).
		Order("g.created_at DESC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// ListByAuction returns the grants given for one auction (staff view).
func (r *FreeBidRepo) ListByAuction(ctx context.Context, auctionID uuid.UUID) ([]domain.FreeBidGrant, error) {
	var rows []domain.FreeBidGrant
	err := r.db.WithContext(ctx).
		Table("free_bid_grants g").
		Select("g.*, COALESCE(p.full_name, '') AS participant_name").
		Joins("LEFT JOIN participant_users p ON p.id = g.participant_id").
		Where("g.auction_id = ?", auctionID).
		Order("g.rank = 0, g.rank ASC, g.bid_value DESC").
		Find(&rows).Error
	return rows, err
}
