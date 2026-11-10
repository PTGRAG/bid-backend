//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/testutil"
	"github.com/google/uuid"
)

func freeAuction(t *testing.T, s *testutil.Stack, enabled bool, limit int) *domain.Auction {
	t.Helper()
	return testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status:              domain.AuctionStatusActive,
		EntranceFee:         domain.NewMoney(50),
		StartInPast:         true,
		FreeEntranceEnabled: enabled,
		FreeEntranceLimit:   limit,
	})
}

func claimedOf(t *testing.T, s *testutil.Stack, auctionID uuid.UUID) int {
	t.Helper()
	a, err := s.Auctions.GetByID(auctionID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return a.FreeEntranceClaimed
}

func reserve(s *testutil.Stack, auctionID, userID uuid.UUID, bids ...float64) (string, error) {
	tx := "R-" + uuid.New().String()[:20]
	pack := make([]domain.Money, 0, len(bids))
	total := domain.Zero()
	for _, b := range bids {
		m := domain.NewMoney(b)
		pack = append(pack, m)
		total = total.Add(m)
	}
	return tx, s.Chapa.CreateWaivedPendingPayment(context.Background(), tx, userID, auctionID, total, pack...)
}

// waivedSnap is what Chapa reports for a free-entrance checkout: only the bid total was charged.
func waivedSnap(tx string, auctionID, userID uuid.UUID, amount, status string) *chapapayment.VerifiedSnapshot {
	snap := chapaSnap(tx, auctionID, userID, amount)
	snap.DataStatus = status
	return snap
}

func TestFreeEntrance_ReservationNeverExceedsLimitUnderContention(t *testing.T) {
	s := stack(t)
	const limit, contenders = 3, 12
	a := freeAuction(t, s, true, limit)

	var wg sync.WaitGroup
	results := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		userID := testutil.SeedParticipant(t, s.PG, domain.Zero())
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reserve(s, a.ID, userID, 1)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	ok, unavailable := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrFreeEntranceUnavailable):
			unavailable++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != limit || unavailable != contenders-limit {
		t.Fatalf("ok=%d unavailable=%d, want %d/%d", ok, unavailable, limit, contenders-limit)
	}
	if got := claimedOf(t, s, a.ID); got != limit {
		t.Fatalf("claimed=%d, want %d", got, limit)
	}
}

func TestFreeEntrance_OneFreeEntrancePerCustomer(t *testing.T) {
	s := stack(t)
	a1 := freeAuction(t, s, true, 100)
	a2 := freeAuction(t, s, true, 100)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())

	tx1, err := reserve(s, a1.ID, user, 2)
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := reserve(s, a2.ID, user, 2); !errors.Is(err, domain.ErrFreeEntranceUnavailable) {
		t.Fatalf("second concurrent free checkout must be refused, got %v", err)
	}
	if got := claimedOf(t, s, a2.ID); got != 0 {
		t.Fatalf("refused reservation must not consume a slot, claimed=%d", got)
	}

	// Releasing the first (abandoned) checkout frees the customer's one free entrance again.
	released, err := s.Ledger.FailPendingWaived(context.Background(), tx1, "test")
	if err != nil || !released {
		t.Fatalf("release: released=%v err=%v", released, err)
	}
	if got := claimedOf(t, s, a1.ID); got != 0 {
		t.Fatalf("release must return the slot, claimed=%d", got)
	}
	if _, err := reserve(s, a2.ID, user, 2); err != nil {
		t.Fatalf("after release the customer may reserve again: %v", err)
	}

	// Releasing twice is a no-op (counter must not go negative or double-refund).
	if released, _ := s.Ledger.FailPendingWaived(context.Background(), tx1, "again"); released {
		t.Fatal("second release must be a no-op")
	}
	if got := claimedOf(t, s, a1.ID); got != 0 {
		t.Fatalf("claimed=%d after double release", got)
	}
}

func TestFreeEntrance_DisabledAndInactiveAuctionsRefuse(t *testing.T) {
	s := stack(t)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())

	off := freeAuction(t, s, false, 100)
	if _, err := reserve(s, off.ID, user, 1); !errors.Is(err, domain.ErrFreeEntranceUnavailable) {
		t.Fatalf("disabled promo: got %v", err)
	}

	draft := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status: domain.AuctionStatusDraft, FreeEntranceEnabled: true, FreeEntranceLimit: 10,
	})
	if _, err := reserve(s, draft.ID, user, 1); !errors.Is(err, domain.ErrFreeEntranceUnavailable) {
		t.Fatalf("draft auction: got %v", err)
	}
}

func TestFreeEntrance_LimitAllHasNoCap(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, domain.FreeEntranceUnlimited)
	for i := 0; i < 25; i++ {
		user := testutil.SeedParticipant(t, s.PG, domain.Zero())
		if _, err := reserve(s, a.ID, user, 1); err != nil {
			t.Fatalf("joiner %d: %v", i, err)
		}
	}
	if got := claimedOf(t, s, a.ID); got != 25 {
		t.Fatalf("claimed=%d, want 25", got)
	}
}

func TestFreeEntrance_PaidCheckoutIsFulfilledAsFreeParticipation(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 100)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())

	// Customer bids 1.00 and 2.50: the fee (50 each) is waived, Chapa charges 3.50 only.
	tx, err := reserve(s, a.ID, user, 1, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Chapa.ProcessVerified(context.Background(), waivedSnap(tx, a.ID, user, "3.50", "success")); err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}

	part, err := s.LUB.GetParticipation(a.ID, user.String())
	if err != nil || part == nil {
		t.Fatalf("participation missing: %v", err)
	}
	if !part.EntranceFree {
		t.Fatal("participation must be flagged entrance_free")
	}
	if part.FeePaid.String() != "3.50" {
		t.Fatalf("fee_paid=%s, want the bid total 3.50", part.FeePaid.String())
	}
	bids, err := s.LUB.ListMyBids(a.ID, user.String())
	if err != nil || len(bids) != 2 {
		t.Fatalf("bids=%d err=%v, want 2", len(bids), err)
	}

	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	if p.Status != domain.PaymentStatusSuccess || !p.Processed || !p.EntranceWaived {
		t.Fatalf("ledger row: %+v", p)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d, want 1 (reserved at checkout, not double counted)", got)
	}

	// Duplicate webhook delivery changes nothing.
	if err := s.Chapa.ProcessVerified(context.Background(), waivedSnap(tx, a.ID, user, "3.50", "success")); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d after duplicate delivery", got)
	}

	// The customer has now used their one free entrance: no more free checkouts anywhere.
	other := freeAuction(t, s, true, 100)
	if _, err := reserve(s, other.ID, user, 1); !errors.Is(err, domain.ErrFreeEntranceUnavailable) {
		t.Fatalf("second free auction must be refused, got %v", err)
	}
}

func TestFreeEntrance_FailedPaymentReleasesSlot(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 1)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx, err := reserve(s, a.ID, user, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d", got)
	}

	if err := s.Chapa.ProcessVerified(context.Background(), waivedSnap(tx, a.ID, user, "1.00", "failed")); err != nil {
		t.Fatalf("ProcessVerified(failed): %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 0 {
		t.Fatalf("failed payment must return the slot, claimed=%d", got)
	}
	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	if p.Status != domain.PaymentStatusFailed || !p.EntranceWaived {
		t.Fatalf("ledger row: %+v", p)
	}

	// The single slot is available to someone else again.
	if _, err := reserve(s, a.ID, testutil.SeedParticipant(t, s.PG, domain.Zero()), 1); err != nil {
		t.Fatalf("slot should be reusable: %v", err)
	}
}

func TestFreeEntrance_LatePaymentAfterReleaseReopensSlot(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 5)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx, err := reserve(s, a.ID, user, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Reconciler/supersede released it ...
	if released, err := s.Ledger.FailPendingWaived(context.Background(), tx, "expired"); err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	// ... but the customer had actually paid.
	if err := s.Chapa.ProcessVerified(context.Background(), waivedSnap(tx, a.ID, user, "4.00", "success")); err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}
	part, _ := s.LUB.GetParticipation(a.ID, user.String())
	if part == nil || !part.EntranceFree {
		t.Fatalf("late payment should still get the free participation: %+v", part)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d, want 1 after reopen", got)
	}
}

func TestFreeEntrance_LatePaymentWhenSlotsGoneIsFulfilledAsNormalAndCountersStayConsistent(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 1)
	late := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx, err := reserve(s, a.ID, late, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ledger.FailPendingWaived(context.Background(), tx, "expired"); err != nil {
		t.Fatal(err)
	}
	// Someone else takes the only slot meanwhile.
	if _, err := reserve(s, a.ID, testutil.SeedParticipant(t, s.PG, domain.Zero()), 1); err != nil {
		t.Fatal(err)
	}

	if err := s.Chapa.ProcessVerified(context.Background(), waivedSnap(tx, a.ID, late, "4.00", "success")); err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}
	part, _ := s.LUB.GetParticipation(a.ID, late.String())
	if part == nil {
		t.Fatal("the paying customer must still be fulfilled")
	}
	if part.EntranceFree {
		t.Fatal("no slot was available, so this must not be flagged free")
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d, want 1 (late payer must not take a second slot)", got)
	}
	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	if p.EntranceWaived {
		t.Fatal("waiver flag must be cleared when the slot could not be re-taken")
	}
}

func TestFreeEntrance_RefundReturnsSlotAndFreesCustomer(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 1)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx, err := reserve(s, a.ID, user, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Chapa.ProcessVerified(context.Background(), waivedSnap(tx, a.ID, user, "2.00", "success")); err != nil {
		t.Fatal(err)
	}
	if err := s.Ledger.MarkRefunded(context.Background(), tx); err != nil {
		t.Fatalf("MarkRefunded: %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 0 {
		t.Fatalf("refund must return the slot, claimed=%d", got)
	}
	// Refunding twice must not return the slot twice.
	if err := s.Ledger.MarkRefunded(context.Background(), tx); err != nil {
		t.Fatalf("second MarkRefunded: %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 0 {
		t.Fatalf("claimed=%d after double refund", got)
	}
}

func TestFreeEntrance_AdminEditCannotClobberTheSlotCounter(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 100)

	stale, err := s.Auctions.GetByID(a.ID) // admin opens the edit form: claimed = 0
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(s, a.ID, testutil.SeedParticipant(t, s.PG, domain.Zero()), 1); err != nil {
		t.Fatal(err)
	}
	stale.Title = "renamed by admin"
	if err := s.Auctions.Update(stale); err != nil { // admin saves with the stale copy
		t.Fatal(err)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("admin save clobbered the counter: claimed=%d, want 1", got)
	}
}

func TestFreeEntrance_DatabaseRejectsImpossibleSettings(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 10)
	for i := 0; i < 3; i++ {
		if _, err := reserve(s, a.ID, testutil.SeedParticipant(t, s.PG, domain.Zero()), 1); err != nil {
			t.Fatal(err)
		}
	}
	// Even a buggy caller cannot push the cap below what is already reserved.
	if err := s.PG.DB.Exec(`UPDATE auctions SET free_entrance_limit = 2 WHERE id = ?`, a.ID).Error; err == nil {
		t.Fatal("limit below claimed must violate chk_auctions_free_entrance")
	}
	if err := s.PG.DB.Exec(`UPDATE auctions SET free_entrance_limit = -1 WHERE id = ?`, a.ID).Error; err == nil {
		t.Fatal("negative limit must violate chk_auctions_free_entrance")
	}
	if err := s.PG.DB.Exec(`UPDATE auctions SET free_entrance_limit = 1000001 WHERE id = ?`, a.ID).Error; err == nil {
		t.Fatal("limit above max must violate chk_auctions_free_entrance")
	}
}

func TestFreeEntrance_AdminLimitEditThroughService(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 10)
	for i := 0; i < 3; i++ {
		if _, err := reserve(s, a.ID, testutil.SeedParticipant(t, s.PG, domain.Zero()), 1); err != nil {
			t.Fatal(err)
		}
	}
	actor := uuid.New()
	low := 2
	err := s.LUB.UpdateAuctionWithFreeEntrance(context.Background(), a.ID, &domain.Auction{}, &lub.FreeEntranceSettings{Limit: &low}, actor)
	if err == nil {
		t.Fatal("limit below the 3 claimed must be rejected")
	}
	all := domain.FreeEntranceUnlimited
	if err := s.LUB.UpdateAuctionWithFreeEntrance(context.Background(), a.ID, &domain.Auction{}, &lub.FreeEntranceSettings{Limit: &all}, actor); err != nil {
		t.Fatalf("switch to all: %v", err)
	}
	off := false
	if err := s.LUB.UpdateAuctionWithFreeEntrance(context.Background(), a.ID, &domain.Auction{}, &lub.FreeEntranceSettings{Enabled: &off}, actor); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ := s.Auctions.GetByID(a.ID)
	if got.FreeEntranceEnabled || got.FreeEntranceClaimed != 3 {
		t.Fatalf("auction after edits: enabled=%v claimed=%d", got.FreeEntranceEnabled, got.FreeEntranceClaimed)
	}
	if _, err := reserve(s, a.ID, testutil.SeedParticipant(t, s.PG, domain.Zero()), 1); !errors.Is(err, domain.ErrFreeEntranceUnavailable) {
		t.Fatalf("after disabling, new free checkouts are refused: %v", err)
	}
}

// fakeChapa answers VerifyTransaction from a per-tx_ref table.
type fakeChapa struct {
	status map[string]string // tx_ref -> chapa status
	meta   map[string]map[string]interface{}
	amount map[string]string
	err    error
}

func (f *fakeChapa) InitializeTransaction(ports.ChapaInitializeRequest) (*ports.ChapaInitializeResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakeChapa) VerifyTransaction(tx string) (*ports.ChapaTransactionSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ports.ChapaTransactionSnapshot{
		TxRef: tx, Status: f.status[tx], Amount: f.amount[tx], Meta: f.meta[tx],
	}, nil
}
func (f *fakeChapa) InitiateRefund(string, float64, string, string) (string, error) { return "", nil }
func (f *fakeChapa) VerifyRefund(string) (string, error)                            { return "", nil }

func TestFreeEntrance_RetryAfterAbandonedCheckoutSupersedesIt(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 5)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())

	// The customer opened a free checkout, closed the Chapa tab (still "pending"), and retries.
	abandoned, err := reserve(s, a.ID, user, 2)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeChapa{status: map[string]string{abandoned: "pending"}}
	s.Chapa.SetChapaClient(fc)

	// A second reservation is blocked until the first is settled ...
	if _, err := reserve(s, a.ID, user, 3); !errors.Is(err, domain.ErrFreeEntranceUnavailable) {
		t.Fatalf("expected refusal while the old checkout is pending, got %v", err)
	}
	// ... which the handler does first: verify with Chapa, release the unpaid one.
	if err := s.Chapa.SettleStaleWaivedCheckouts(context.Background(), user); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 0 {
		t.Fatalf("abandoned checkout must release its slot, claimed=%d", got)
	}
	if _, err := reserve(s, a.ID, user, 3); err != nil {
		t.Fatalf("retry after settle: %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d, want exactly 1 (not one per attempt)", got)
	}
}

func TestFreeEntrance_SettleFulfilsAnEarlierCheckoutThatWasActuallyPaid(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 5)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())
	paid, err := reserve(s, a.ID, user, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.Chapa.SetChapaClient(&fakeChapa{
		status: map[string]string{paid: "success"},
		amount: map[string]string{paid: "2.00"},
		meta:   map[string]map[string]interface{}{paid: {"auction_id": a.ID.String(), "user_id": user.String()}},
	})

	if err := s.Chapa.SettleStaleWaivedCheckouts(context.Background(), user); err != nil {
		t.Fatalf("settle: %v", err)
	}
	part, _ := s.LUB.GetParticipation(a.ID, user.String())
	if part == nil || !part.EntranceFree {
		t.Fatalf("paid checkout should have been fulfilled as free participation: %+v", part)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("claimed=%d, want 1", got)
	}
}

func TestFreeEntrance_SettleRefusesWhenChapaCannotConfirmRecentCheckout(t *testing.T) {
	s := stack(t)
	a := freeAuction(t, s, true, 5)
	user := testutil.SeedParticipant(t, s.PG, domain.Zero())
	if _, err := reserve(s, a.ID, user, 2); err != nil {
		t.Fatal(err)
	}
	s.Chapa.SetChapaClient(&fakeChapa{err: errors.New("chapa down")})

	err := s.Chapa.SettleStaleWaivedCheckouts(context.Background(), user)
	var app *domain.AppError
	if !errors.As(err, &app) || app.Code != 409 {
		t.Fatalf("want 409 (don't guess about a recent payment), got %v", err)
	}
	if got := claimedOf(t, s, a.ID); got != 1 {
		t.Fatalf("slot must stay reserved when status is unknown, claimed=%d", got)
	}
}
