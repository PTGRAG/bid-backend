//go:build integration

package testutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

func SeedParticipant(t testing.TB, pg *PG, _ domain.Money) uuid.UUID {
	t.Helper()
	id := uuid.New()
	user := &domain.ParticipantUser{
		ID:        id.String(),
		Email:     fmt.Sprintf("user-%s@test.local", id.String()[:8]),
		FullName:  "Test Participant",
		Phone:     "+251922000000",
		Status:    "active",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := pg.DB.Create(user).Error; err != nil {
		t.Fatalf("SeedParticipant: %v", err)
	}
	return id
}

type AuctionOpts struct {
	Status      domain.AuctionStatus
	EntranceFee domain.Money
	MinBidValue domain.Money
	MaxBidValue *domain.Money
	StartInPast bool

	// Free-entrance promo (off by default, like production).
	FreeEntranceEnabled bool
	FreeEntranceLimit   int // 0 = "all"
	FreeEntranceClaimed int
}

func SeedAuction(t testing.TB, pg *PG, opts AuctionOpts) *domain.Auction {
	t.Helper()
	if opts.Status == "" {
		opts.Status = domain.AuctionStatusDraft
	}
	if opts.EntranceFee.IsZero() {
		opts.EntranceFee = domain.NewMoney(10)
	}
	if opts.MinBidValue.IsZero() {
		opts.MinBidValue = domain.NewMoney(0.01)
	}
	var start *time.Time
	closeAt := time.Now().UTC().Add(24 * time.Hour)
	if opts.StartInPast {
		t0 := time.Now().UTC().Add(-time.Hour)
		start = &t0
	}
	a := &domain.Auction{
		ID:          uuid.New(),
		Title:       "Test Auction " + uuid.New().String()[:6],
		Description: "Integration test auction",
		Status:      opts.Status,
		EntranceFee: opts.EntranceFee,
		MinBidValue: opts.MinBidValue,
		MaxBidValue: opts.MaxBidValue,
		StartAt:     start,
		CloseAt:     &closeAt,
		CreatedBy:   uuid.New(),

		FreeEntranceEnabled: opts.FreeEntranceEnabled,
		FreeEntranceLimit:   opts.FreeEntranceLimit,
		FreeEntranceClaimed: opts.FreeEntranceClaimed,
	}
	if err := pg.DB.Create(a).Error; err != nil {
		t.Fatalf("SeedAuction: %v", err)
	}
	return a
}

func SeedAuctionParticipation(t testing.TB, pg *PG, auctionID, userID uuid.UUID) *domain.AuctionParticipation {
	t.Helper()
	p := &domain.AuctionParticipation{
		ID:            uuid.New(),
		AuctionID:     auctionID,
		ParticipantID: userID.String(),
		Status:        domain.ParticipationStatusActive,
		TransactionID: "TX-" + uuid.New().String(),
		FeePaid:       domain.NewMoney(10),
	}
	if err := pg.DB.Create(p).Error; err != nil {
		t.Fatalf("SeedAuctionParticipation: %v", err)
	}
	return p
}

func SeedBid(t testing.TB, pg *PG, auctionID uuid.UUID, participantID string, amount domain.Money) *domain.Bid {
	t.Helper()
	b := &domain.Bid{
		ID:            uuid.New(),
		AuctionID:     auctionID,
		ParticipantID: participantID,
		BidValue:      amount,
	}
	if err := pg.DB.Create(b).Error; err != nil {
		t.Fatalf("SeedBid: %v", err)
	}
	return b
}

func SeedAdminUser(t testing.TB, pg *PG) *domain.User {
	t.Helper()
	user := &domain.User{
		ID:        uuid.New(),
		FullName:  "Test Admin",
		Email:     fmt.Sprintf("admin-%s@test.local", uuid.New().String()[:8]),
		Password:  "hashed-placeholder",
		Phone:     "+251911000000",
		Role:      domain.RoleAdmin,
		Status:    domain.StatusActive,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := pg.DB.Create(user).Error; err != nil {
		t.Fatalf("SeedAdminUser: %v", err)
	}
	return user
}
