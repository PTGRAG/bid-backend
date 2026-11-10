package lub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

func intPtr(v int) *int             { return &v }
func boolPtr(v bool) *bool          { return &v }
func noneUsed(string) (bool, error) { return false, nil }

func auctionRepoReturning(a *domain.Auction) *mockAuctionRepo {
	return &mockAuctionRepo{getFn: func(uuid.UUID) (*domain.Auction, error) {
		cp := *a
		return &cp, nil
	}}
}

func TestFreeEntranceStatus(t *testing.T) {
	closeAt := time.Now().UTC().Add(time.Hour)
	base := domain.Auction{
		ID: uuid.New(), Status: domain.AuctionStatusActive, CloseAt: &closeAt,
		FreeEntranceEnabled: true, FreeEntranceLimit: 100, FreeEntranceClaimed: 10,
	}
	user := uuid.New().String()

	cases := []struct {
		name         string
		mutate       func(a *domain.Auction)
		part         *mockPartRepo
		viewer       string
		wantEligible bool
		wantRemain   int
	}{
		{"eligible", func(a *domain.Auction) {}, &mockPartRepo{hasUsedFreeFn: noneUsed}, user, true, 90},
		{"promo disabled by admin", func(a *domain.Auction) { a.FreeEntranceEnabled = false }, &mockPartRepo{hasUsedFreeFn: noneUsed}, user, false, 0},
		{"slots full", func(a *domain.Auction) { a.FreeEntranceClaimed = 100 }, &mockPartRepo{hasUsedFreeFn: noneUsed}, user, false, 0},
		{"limit all stays open", func(a *domain.Auction) { a.FreeEntranceLimit = 0; a.FreeEntranceClaimed = 99999 }, &mockPartRepo{hasUsedFreeFn: noneUsed}, user, true, 0},
		{"already used free on another auction", func(a *domain.Auction) {}, &mockPartRepo{hasUsedFreeFn: func(string) (bool, error) { return true, nil }}, user, false, 90},
		{"already joined this auction", func(a *domain.Auction) {}, &mockPartRepo{
			hasUsedFreeFn: noneUsed,
			getFn: func(uuid.UUID, string) (*domain.AuctionParticipation, error) {
				return &domain.AuctionParticipation{Status: domain.ParticipationStatusActive}, nil
			},
		}, user, false, 90},
		{"refunded participation can rejoin free", func(a *domain.Auction) {}, &mockPartRepo{
			hasUsedFreeFn: noneUsed,
			getFn: func(uuid.UUID, string) (*domain.AuctionParticipation, error) {
				return &domain.AuctionParticipation{Status: domain.ParticipationStatusRefunded}, nil
			},
		}, user, true, 90},
		{"anonymous", func(a *domain.Auction) {}, &mockPartRepo{hasUsedFreeFn: noneUsed}, "  ", false, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			tc.mutate(&a)
			svc := newSvc(auctionRepoReturning(&a), tc.part, &mockBidRepo{})
			remaining, eligible, err := svc.FreeEntranceStatus(a.ID, tc.viewer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if eligible != tc.wantEligible || remaining != tc.wantRemain {
				t.Fatalf("eligible=%v remaining=%d, want eligible=%v remaining=%d",
					eligible, remaining, tc.wantEligible, tc.wantRemain)
			}
		})
	}
}

func TestFreeEntranceStatus_LookupErrorIsNeverEligible(t *testing.T) {
	a := domain.Auction{ID: uuid.New(), FreeEntranceEnabled: true, FreeEntranceLimit: 100}
	svc := newSvc(auctionRepoReturning(&a),
		&mockPartRepo{hasUsedFreeFn: func(string) (bool, error) { return false, errors.New("db down") }}, &mockBidRepo{})
	_, eligible, err := svc.FreeEntranceStatus(a.ID, uuid.New().String())
	if err == nil || eligible {
		t.Fatalf("expected error and not eligible, got eligible=%v err=%v", eligible, err)
	}

	// Enrichment (used for API responses) degrades to "not eligible" instead of promising.
	enriched := a
	svc.EnrichAuctionFreeEntrance(&enriched, uuid.New().String())
	if enriched.UserEligibleForFreeEntrance {
		t.Fatal("must not promise a free entrance when the lookup failed")
	}
}

func TestEnrichAuctionFreeEntrance_UnlimitedAndJoined(t *testing.T) {
	svc := newSvc(&mockAuctionRepo{}, &mockPartRepo{hasUsedFreeFn: noneUsed}, &mockBidRepo{})

	all := &domain.Auction{ID: uuid.New(), FreeEntranceEnabled: true, FreeEntranceLimit: 0, FreeEntranceClaimed: 3}
	svc.EnrichAuctionFreeEntrance(all, uuid.New().String())
	if !all.FreeEntranceUnlimited || !all.UserEligibleForFreeEntrance || all.FreeEntranceRemaining != 0 {
		t.Fatalf("unlimited promo: %+v", all)
	}

	joined := &domain.Auction{ID: uuid.New(), FreeEntranceEnabled: true, FreeEntranceLimit: 5, UserHasJoined: true}
	svc.EnrichAuctionFreeEntrance(joined, uuid.New().String())
	if joined.UserEligibleForFreeEntrance {
		t.Fatal("a customer who already joined is not eligible")
	}

	off := &domain.Auction{ID: uuid.New(), FreeEntranceEnabled: false, FreeEntranceLimit: 5}
	svc.EnrichAuctionFreeEntrance(off, uuid.New().String())
	if off.UserEligibleForFreeEntrance || off.FreeEntranceUnlimited || off.FreeEntranceRemaining != 0 {
		t.Fatalf("disabled promo must expose nothing: %+v", off)
	}
}

func TestCreateAuction_FreeEntranceValidation(t *testing.T) {
	create := func(a *domain.Auction) (*domain.Auction, error) {
		var stored *domain.Auction
		ar := &mockAuctionRepo{createFn: func(x *domain.Auction) error { cp := *x; stored = &cp; return nil }}
		err := newSvc(ar, &mockPartRepo{}, &mockBidRepo{}).CreateAuction(context.Background(), a, uuid.New())
		return stored, err
	}

	got, err := create(&domain.Auction{Title: "x", FreeEntranceEnabled: true, FreeEntranceLimit: 250, FreeEntranceClaimed: 77})
	if err != nil {
		t.Fatal(err)
	}
	if !got.FreeEntranceEnabled || got.FreeEntranceLimit != 250 {
		t.Fatalf("settings not preserved: %+v", got)
	}
	if got.FreeEntranceClaimed != 0 {
		t.Fatalf("new auction must start with 0 claimed, got %d", got.FreeEntranceClaimed)
	}

	// Disabled stays disabled (no implicit promo).
	got, err = create(&domain.Auction{Title: "x", FreeEntranceEnabled: false, FreeEntranceLimit: 100})
	if err != nil || got.FreeEntranceEnabled {
		t.Fatalf("disabled promo must stay disabled: %+v err=%v", got, err)
	}

	// "all" is stored as 0.
	got, err = create(&domain.Auction{Title: "x", FreeEntranceEnabled: true, FreeEntranceLimit: domain.FreeEntranceUnlimited})
	if err != nil || !got.FreeEntranceIsUnlimited() {
		t.Fatalf("limit all: %+v err=%v", got, err)
	}

	for _, bad := range []int{-1, domain.MaxFreeEntranceLimit + 1} {
		stored, err := create(&domain.Auction{Title: "x", FreeEntranceEnabled: true, FreeEntranceLimit: bad})
		if err == nil || stored != nil {
			t.Fatalf("limit %d must be rejected before persisting (err=%v stored=%v)", bad, err, stored)
		}
		var app *domain.AppError
		if !errors.As(err, &app) || app.Code != 422 {
			t.Fatalf("limit %d: want 422 AppError, got %v", bad, err)
		}
	}
}

func TestUpdateAuctionWithFreeEntrance(t *testing.T) {
	closeAt := time.Now().UTC().Add(time.Hour)
	existing := func() *domain.Auction {
		return &domain.Auction{
			ID: uuid.New(), Title: "t", Status: domain.AuctionStatusActive, CloseAt: &closeAt,
			FreeEntranceEnabled: true, FreeEntranceLimit: 100, FreeEntranceClaimed: 40,
		}
	}
	run := func(a *domain.Auction, free *FreeEntranceSettings) (*domain.Auction, error) {
		var saved *domain.Auction
		ar := &mockAuctionRepo{
			getFn:    func(uuid.UUID) (*domain.Auction, error) { cp := *a; return &cp, nil },
			updateFn: func(x *domain.Auction) error { cp := *x; saved = &cp; return nil },
		}
		err := newSvc(ar, &mockPartRepo{}, &mockBidRepo{}).
			UpdateAuctionWithFreeEntrance(context.Background(), a.ID, &domain.Auction{}, free, uuid.New())
		return saved, err
	}

	t.Run("nil settings leave the promo untouched", func(t *testing.T) {
		saved, err := run(existing(), nil)
		if err != nil || saved.FreeEntranceEnabled != true || saved.FreeEntranceLimit != 100 {
			t.Fatalf("saved=%+v err=%v", saved, err)
		}
	})
	t.Run("admin can disable", func(t *testing.T) {
		saved, err := run(existing(), &FreeEntranceSettings{Enabled: boolPtr(false)})
		if err != nil || saved.FreeEntranceEnabled || saved.FreeEntranceLimit != 100 {
			t.Fatalf("saved=%+v err=%v", saved, err)
		}
	})
	t.Run("admin can raise the limit", func(t *testing.T) {
		saved, err := run(existing(), &FreeEntranceSettings{Limit: intPtr(1000)})
		if err != nil || saved.FreeEntranceLimit != 1000 || !saved.FreeEntranceEnabled {
			t.Fatalf("saved=%+v err=%v", saved, err)
		}
	})
	t.Run("admin can set all", func(t *testing.T) {
		saved, err := run(existing(), &FreeEntranceSettings{Limit: intPtr(domain.FreeEntranceUnlimited)})
		if err != nil || !saved.FreeEntranceIsUnlimited() {
			t.Fatalf("saved=%+v err=%v", saved, err)
		}
	})
	t.Run("limit may equal claimed", func(t *testing.T) {
		saved, err := run(existing(), &FreeEntranceSettings{Limit: intPtr(40)})
		if err != nil || saved.FreeEntranceLimit != 40 {
			t.Fatalf("saved=%+v err=%v", saved, err)
		}
	})
	t.Run("limit below claimed is rejected and nothing is saved", func(t *testing.T) {
		saved, err := run(existing(), &FreeEntranceSettings{Limit: intPtr(39)})
		var app *domain.AppError
		if !errors.As(err, &app) || app.Code != 422 || saved != nil {
			t.Fatalf("want 422 and no write, got saved=%+v err=%v", saved, err)
		}
	})
	t.Run("out-of-range limits are rejected", func(t *testing.T) {
		for _, bad := range []int{-5, domain.MaxFreeEntranceLimit + 1} {
			saved, err := run(existing(), &FreeEntranceSettings{Limit: intPtr(bad)})
			if err == nil || saved != nil {
				t.Fatalf("limit %d: want rejection, got saved=%+v err=%v", bad, saved, err)
			}
		}
	})
	t.Run("executed auctions are immutable", func(t *testing.T) {
		a := existing()
		a.Status = domain.AuctionStatusExecuted
		saved, err := run(a, &FreeEntranceSettings{Enabled: boolPtr(false)})
		if err == nil || saved != nil {
			t.Fatalf("want rejection, got saved=%+v err=%v", saved, err)
		}
	})
}

func TestRecordFreeEntranceParticipation(t *testing.T) {
	closeAt := time.Now().UTC().Add(time.Hour)
	a := &domain.Auction{ID: uuid.New(), Status: domain.AuctionStatusActive, CloseAt: &closeAt}
	var created []*domain.AuctionParticipation
	pa := &mockPartRepo{createFn: func(p *domain.AuctionParticipation) error { cp := *p; created = append(created, &cp); return nil }}
	svc := newSvc(auctionRepoReturning(a), pa, &mockBidRepo{})

	// The customer paid only their bid (3.50), never the entrance fee.
	part, err := svc.RecordFreeEntranceParticipation(context.Background(), a.ID, uuid.New().String(), "R-free-1", domain.NewMoney(3.5))
	if err != nil {
		t.Fatal(err)
	}
	if !part.EntranceFree || part.FeePaid.String() != "3.50" || part.Status != domain.ParticipationStatusActive {
		t.Fatalf("free participation: %+v", part)
	}

	// A normal paid participation is never flagged free.
	paid, err := svc.RecordParticipation(context.Background(), a.ID, uuid.New().String(), "R-paid-1", domain.NewMoney(53.5))
	if err != nil {
		t.Fatal(err)
	}
	if paid.EntranceFree {
		t.Fatal("paid participation must not be flagged free")
	}
	if len(created) != 2 {
		t.Fatalf("expected 2 participations, got %d", len(created))
	}
}
