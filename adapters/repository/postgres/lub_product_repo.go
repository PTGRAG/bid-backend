package postgres

import (
	"errors"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AuctionRepository implements ports.AuctionRepository.
type AuctionRepository struct {
	db *gorm.DB
}

func NewAuctionRepository(db *gorm.DB) *AuctionRepository {
	return &AuctionRepository{db: db}
}

func (r *AuctionRepository) Create(a *domain.Auction) error {
	return r.db.Create(a).Error
}

func (r *AuctionRepository) GetByID(id uuid.UUID) (*domain.Auction, error) {
	var a domain.Auction
	err := r.db.First(&a, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	slice := []domain.Auction{a}
	r.populateWinnerNames(slice)
	a = slice[0]
	return &a, nil
}

func (r *AuctionRepository) Update(a *domain.Auction) error {
	// free_entrance_claimed is owned by the payment ledger (atomic reserve/release). Saving a
	// stale in-memory copy of it would silently corrupt the slot counter.
	return r.db.Omit("free_entrance_claimed").Save(a).Error
}

func (r *AuctionRepository) UpdateStatus(id uuid.UUID, status domain.AuctionStatus) error {
	return r.db.Model(&domain.Auction{}).Where("id = ?", id).
		Update("status", status).Error
}

func (r *AuctionRepository) List(status, search string, limit, offset int) ([]domain.Auction, int64, error) {
	q := r.db.Model(&domain.Auction{})
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if strings.TrimSpace(search) != "" {
		q = q.Where("title ILIKE ?", "%"+strings.TrimSpace(search)+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var auctions []domain.Auction
	if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&auctions).Error; err != nil {
		return nil, 0, err
	}
	r.populateWinnerNames(auctions)
	return auctions, total, nil
}

func (r *AuctionRepository) ListPublic(limit, offset int) ([]domain.Auction, int64, error) {
	auctions, total, _, err := r.ListPublicFiltered(ports.PublicAuctionFilter{
		Limit:  limit,
		Offset: offset,
	})
	return auctions, total, err
}

func (r *AuctionRepository) ListPublicFiltered(filter ports.PublicAuctionFilter) ([]domain.Auction, int64, map[string]int64, error) {
	publicStatuses := []string{
		string(domain.AuctionStatusActive),
		string(domain.AuctionStatusPostponed),
		string(domain.AuctionStatusClosed),
		string(domain.AuctionStatusExecuted),
	}

	// Compute status counts (filtered by search if provided, across all public statuses)
	statusCounts := map[string]int64{
		"all":      0,
		"active":   0,
		"closed":   0,
		"executed": 0,
	}
	countBase := r.db.Model(&domain.Auction{}).Where("status IN ?", publicStatuses)
	if s := strings.TrimSpace(filter.Search); s != "" {
		like := "%" + s + "%"
		countBase = countBase.Where("(title ILIKE ? OR description ILIKE ?)", like, like)
	}
	type statusCountRow struct {
		Status string `gorm:"column:status"`
		Total  int64  `gorm:"column:total"`
	}
	var scRows []statusCountRow
	if err := countBase.Select("status, COUNT(*) AS total").Group("status").Scan(&scRows).Error; err == nil {
		var allCount int64
		for _, row := range scRows {
			allCount += row.Total
			switch strings.ToLower(strings.TrimSpace(row.Status)) {
			case "active", "postponed":
				statusCounts["active"] += row.Total
			case "closed":
				statusCounts["closed"] += row.Total
			case "executed":
				statusCounts["executed"] += row.Total
			}
		}
		statusCounts["all"] = allCount
	}

	q := r.db.Model(&domain.Auction{})
	switch strings.ToLower(strings.TrimSpace(filter.Status)) {
	case "active", "live":
		q = q.Where("status IN ?", []string{
			string(domain.AuctionStatusActive),
			string(domain.AuctionStatusPostponed),
		})
	case "postponed":
		q = q.Where("status = ?", string(domain.AuctionStatusPostponed))
	case "closed":
		q = q.Where("status = ?", string(domain.AuctionStatusClosed))
	case "executed", "completed":
		q = q.Where("status = ?", string(domain.AuctionStatusExecuted))
	default:
		q = q.Where("status IN ?", publicStatuses)
	}

	if s := strings.TrimSpace(filter.Search); s != "" {
		like := "%" + s + "%"
		q = q.Where("(title ILIKE ? OR description ILIKE ?)", like, like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, statusCounts, err
	}

	switch strings.ToLower(strings.TrimSpace(filter.Sort)) {
	case "newest":
		q = q.Order("created_at DESC")
	case "closing_soon":
		q = q.Order("CASE WHEN status IN ('active', 'postponed') THEN 0 ELSE 1 END ASC").
			Order("close_at ASC NULLS LAST").
			Order("created_at DESC")
	case "fee_asc":
		q = q.Order("entrance_fee ASC").Order("created_at DESC")
	case "fee_desc":
		q = q.Order("entrance_fee DESC").Order("created_at DESC")
	default:
		// Prioritize live auctions first, then highest participant count, then newest.
		q = q.
			Order("CASE WHEN status IN ('active', 'postponed') THEN 0 WHEN status = 'closed' THEN 1 ELSE 2 END ASC").
			Order(`(
				SELECT COUNT(*) FROM auction_participations ap
				WHERE ap.auction_id = auctions.id AND ap.status = 'active'
			) DESC`).
			Order("created_at DESC")
	}

	var auctions []domain.Auction
	if err := q.Limit(filter.Limit).Offset(filter.Offset).Find(&auctions).Error; err != nil {
		return nil, 0, statusCounts, err
	}
	r.populateWinnerNames(auctions)
	return auctions, total, statusCounts, nil
}

func (r *AuctionRepository) populateWinnerNames(auctions []domain.Auction) {
	if len(auctions) == 0 {
		return
	}

	auctionIDs := make([]uuid.UUID, 0, len(auctions))
	idSet := make(map[string]struct{})
	for _, a := range auctions {
		if a.ID != uuid.Nil {
			auctionIDs = append(auctionIDs, a.ID)
		}
		if a.WinnerParticipantID != nil {
			pid := strings.TrimSpace(*a.WinnerParticipantID)
			if pid != "" {
				idSet[pid] = struct{}{}
			}
		}
	}

	if len(auctionIDs) > 0 {
		type bidCountRow struct {
			AuctionID uuid.UUID `gorm:"column:auction_id"`
			Total     int64     `gorm:"column:total"`
		}
		var countRows []bidCountRow
		if err := r.db.Table("auction_bids").
			Select("auction_id, COUNT(*) AS total").
			Where("auction_id IN ?", auctionIDs).
			Group("auction_id").
			Scan(&countRows).Error; err == nil {
			countMap := make(map[uuid.UUID]int64, len(countRows))
			for _, cr := range countRows {
				countMap[cr.AuctionID] = cr.Total
			}
			for i := range auctions {
				auctions[i].TotalBids = countMap[auctions[i].ID]
			}
		}
	}

	if len(idSet) == 0 {
		return
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	nameByID := make(map[string]string, len(ids))

	type idNameRow struct {
		ID       string `gorm:"column:id"`
		FullName string `gorm:"column:full_name"`
	}

	var userRows []idNameRow
	if err := r.db.Table("users").Select("id, full_name").Where("id IN ?", ids).Scan(&userRows).Error; err == nil {
		for _, row := range userRows {
			if n := strings.TrimSpace(row.FullName); n != "" {
				nameByID[strings.ToLower(strings.TrimSpace(row.ID))] = n
			}
		}
	}

	var partRows []idNameRow
	if err := r.db.Table("participant_users").Select("id, full_name").Where("id IN ?", ids).Scan(&partRows).Error; err == nil {
		for _, row := range partRows {
			key := strings.ToLower(strings.TrimSpace(row.ID))
			if _, exists := nameByID[key]; !exists {
				if n := strings.TrimSpace(row.FullName); n != "" {
					nameByID[key] = n
				}
			}
		}
	}

	for i := range auctions {
		if auctions[i].WinnerParticipantID != nil {
			key := strings.ToLower(strings.TrimSpace(*auctions[i].WinnerParticipantID))
			if name, ok := nameByID[key]; ok {
				auctions[i].WinnerName = name
			}
		}
	}
}

func (r *AuctionRepository) ListDueForClose() ([]domain.Auction, error) {
	var auctions []domain.Auction
	now := time.Now().UTC()
	err := r.db.
		Where("status IN ? AND close_at IS NOT NULL AND close_at <= ?",
			[]string{string(domain.AuctionStatusActive), string(domain.AuctionStatusPostponed)},
			now).
		Find(&auctions).Error
	return auctions, err
}

func (r *AuctionRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&domain.Auction{}, "id = ?", id).Error
}
