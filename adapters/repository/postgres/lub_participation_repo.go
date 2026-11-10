package postgres

import (
	"errors"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AuctionParticipationRepository implements ports.AuctionParticipationRepository.
type AuctionParticipationRepository struct {
	db *gorm.DB
}

func NewAuctionParticipationRepository(db *gorm.DB) *AuctionParticipationRepository {
	return &AuctionParticipationRepository{db: db}
}

func (r *AuctionParticipationRepository) Create(p *domain.AuctionParticipation) error {
	return r.db.Create(p).Error
}

func (r *AuctionParticipationRepository) GetByAuctionAndParticipant(auctionID uuid.UUID, participantID string) (*domain.AuctionParticipation, error) {
	var p domain.AuctionParticipation
	err := r.db.Where("auction_id = ? AND participant_id = ?", auctionID, participantID).First(&p).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (r *AuctionParticipationRepository) GetByTransactionID(txID string) (*domain.AuctionParticipation, error) {
	var p domain.AuctionParticipation
	err := r.db.Where("transaction_id = ?", txID).First(&p).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (r *AuctionParticipationRepository) ListByAuction(auctionID uuid.UUID, limit, offset int) ([]domain.AuctionParticipation, int64, error) {
	var total int64
	if err := r.db.Model(&domain.AuctionParticipation{}).Where("auction_id = ?", auctionID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var parts []domain.AuctionParticipation
	if err := r.db.Where("auction_id = ?", auctionID).Order("created_at DESC").Limit(limit).Offset(offset).Find(&parts).Error; err != nil {
		return nil, 0, err
	}
	return parts, total, nil
}

func (r *AuctionParticipationRepository) ListByParticipant(participantID string, limit, offset int) ([]domain.AuctionParticipation, int64, error) {
	var total int64
	if err := r.db.Model(&domain.AuctionParticipation{}).Where("participant_id = ?", participantID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var parts []domain.AuctionParticipation
	if err := r.db.Where("participant_id = ?", participantID).Order("created_at DESC").Limit(limit).Offset(offset).Find(&parts).Error; err != nil {
		return nil, 0, err
	}
	return parts, total, nil
}

func (r *AuctionParticipationRepository) UpdateStatus(id uuid.UUID, status string) error {
	return r.db.Model(&domain.AuctionParticipation{}).Where("id = ?", id).Update("status", status).Error
}

func (r *AuctionParticipationRepository) CountByAuction(auctionID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&domain.AuctionParticipation{}).Where("auction_id = ? AND status = 'active'", auctionID).Count(&count).Error
	return count, err
}

func (r *AuctionParticipationRepository) CountByAuctionSince(auctionID uuid.UUID, since time.Time) (int64, error) {
	var count int64
	err := r.db.Model(&domain.AuctionParticipation{}).
		Where("auction_id = ? AND status = 'active' AND created_at >= ?", auctionID, since).
		Count(&count).Error
	return count, err
}

func (r *AuctionParticipationRepository) CountByAuctions(auctionIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	out := make(map[uuid.UUID]int64, len(auctionIDs))
	if len(auctionIDs) == 0 {
		return out, nil
	}
	type countRow struct {
		AuctionID uuid.UUID `gorm:"column:auction_id"`
		Total     int64     `gorm:"column:total"`
	}
	var rows []countRow
	if err := r.db.Table("auction_participations").
		Select("auction_id, COUNT(*) AS total").
		Where("auction_id IN ? AND status = 'active'", auctionIDs).
		Group("auction_id").
		Scan(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		out[row.AuctionID] = row.Total
	}
	return out, nil
}

func (r *AuctionParticipationRepository) CountByAuctionsSince(auctionIDs []uuid.UUID, since time.Time) (map[uuid.UUID]int64, error) {
	out := make(map[uuid.UUID]int64, len(auctionIDs))
	if len(auctionIDs) == 0 {
		return out, nil
	}
	type countRow struct {
		AuctionID uuid.UUID `gorm:"column:auction_id"`
		Total     int64     `gorm:"column:total"`
	}
	var rows []countRow
	if err := r.db.Table("auction_participations").
		Select("auction_id, COUNT(*) AS total").
		Where("auction_id IN ? AND status = 'active' AND created_at >= ?", auctionIDs, since).
		Group("auction_id").
		Scan(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		out[row.AuctionID] = row.Total
	}
	return out, nil
}

func (r *AuctionParticipationRepository) HasUsedFreeEntrance(participantID string) (bool, error) {
	if strings.TrimSpace(participantID) == "" {
		return false, nil
	}
	var count int64
	err := r.db.Model(&domain.AuctionParticipation{}).
		Where("participant_id = ? AND entrance_free = true AND status = ?", participantID, domain.ParticipationStatusActive).
		Count(&count).Error
	return count > 0, err
}

func (r *AuctionParticipationRepository) JoinedAuctionSet(auctionIDs []uuid.UUID, participantID string) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if len(auctionIDs) == 0 || participantID == "" {
		return out, nil
	}
	var ids []uuid.UUID
	if err := r.db.Table("auction_participations").
		Where("auction_id IN ? AND participant_id = ? AND status = 'active'", auctionIDs, participantID).
		Pluck("auction_id", &ids).Error; err != nil {
		return out, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (r *AuctionParticipationRepository) GetCountByMonth(lastMonths int) ([]domain.MonthlyCount, error) {
	if lastMonths <= 0 {
		lastMonths = 12
	}
	cutoff := time.Now().AddDate(0, -lastMonths, 0)
	var result []domain.MonthlyCount
	err := r.db.Model(&domain.AuctionParticipation{}).
		Select("to_char(created_at, 'YYYY-MM') as month, count(*) as count").
		Where("status = ? AND created_at >= ?", domain.ParticipationStatusActive, cutoff).
		Group("to_char(created_at, 'YYYY-MM')").
		Order("month").
		Scan(&result).Error
	return result, err
}
