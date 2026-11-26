//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/testutil"
	"github.com/google/uuid"
)

// Near-miss free bids: once the winner pays, the next lowest unique bids get one free bid each,
// spent on a later checkout and given back when that checkout fails.

// paidAuction draws an auction and has its winner pay.
func paidAuction(t *testing.T, s *testutil.Stack, bids [][]float64) (*domain.Auction, []string) {
	t.Helper()
	a, ids := drawnAuction(t, s, bids)
	o := openOffer(t, s, a.ID)
	if err := s.Chapa.ProcessVerified(context.Background(), chapaSuccess(winnerCheckout(t, s, o), o.BidValue.String())); err != nil {
		t.Fatalf("winner payment: %v", err)
	}
	return a, ids
}

func balance(t *testing.T, s *testutil.Stack, id string) int {
	t.Helper()
	b, err := s.LUB.FreeBidBalance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Available
}

func TestNearMiss_WinnerPaymentCreditsRunnerUpsOnce(t *testing.T) {
	s := stack(t)
	// 0 wins with 3; 1..4 are the 2nd to 5th lowest unique; 5 is 6th; 6 and 7 tie on 1.
	a, ids := paidAuction(t, s, [][]float64{{3}, {4}, {5}, {6}, {7}, {8}, {1}, {1}})

	for i, want := range []int{0, 1, 1, 1, 1, 0, 0, 0} {
		if got := balance(t, s, ids[i]); got != want {
			t.Fatalf("participant %d balance = %d, want %d", i, got, want)
		}
	}
	if n := countNotifications(t, s, ids[1], domain.NotificationTypeFreeBid); n != 1 {
		t.Fatalf("free bid notifications = %d, want 1", n)
	}

	// Re-running settlement (staff button, retried payment) never credits twice.
	again, err := s.LUB.CreditNearMisses(context.Background(), a.ID, uuid.Nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second credit = %d grants, err %v; want 0", len(again), err)
	}
	if got := balance(t, s, ids[1]); got != 1 {
		t.Fatalf("balance after re-run = %d, want 1", got)
	}

	audit, err := s.LUB.GetAuctionAudit(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for _, e := range audit.Entries {
		if e.FreeBid {
			flagged++
		}
		if e.IsUnique && e.Bidder == "" {
			t.Fatalf("unique bid %s has no masked bidder", e.BidValue)
		}
		if !e.IsUnique && e.Bidder != "" {
			t.Fatal("duplicate bids must not name anyone")
		}
	}
	if flagged != 4 {
		t.Fatalf("audit flags %d free bids, want 4", flagged)
	}
}

func TestNearMiss_SettingsChangeCountAndDuplicates(t *testing.T) {
	s := stack(t)
	ctx := context.Background()
	if _, err := s.LUB.UpdateNearMissSettings(ctx, domain.NearMissSettings{Enabled: true, RunnerUps: 1, IncludeDuplicates: true, MaxDuplicates: 5}, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.LUB.UpdateNearMissSettings(ctx, domain.DefaultNearMissSettings(), uuid.Nil) })

	_, ids := paidAuction(t, s, [][]float64{{3}, {4}, {5}, {1}, {1}})
	for i, want := range []int{0, 1, 0, 1, 1} {
		if got := balance(t, s, ids[i]); got != want {
			t.Fatalf("participant %d balance = %d, want %d", i, got, want)
		}
	}
}

func TestNearMiss_NoCreditBeforeWinnerPays(t *testing.T) {
	s := stack(t)
	a, ids := drawnAuction(t, s, [][]float64{{3}, {4}})
	if _, err := s.LUB.CreditNearMisses(context.Background(), a.ID, uuid.Nil); err == nil {
		t.Fatal("credit must wait for the winner's payment")
	}
	if got := balance(t, s, ids[1]); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestNearMiss_FreeBidPaysTicketAndFailedCheckoutReturnsIt(t *testing.T) {
	s := stack(t)
	ctx := context.Background()
	_, ids := paidAuction(t, s, [][]float64{{3}, {4}})
	user := uuid.MustParse(ids[1])
	if got := balance(t, s, ids[1]); got != 1 {
		t.Fatalf("balance = %d, want 1", got)
	}

	next := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive, EntranceFee: m(50)})

	// Two tickets, one free bid: pays one entrance fee. Chapa declines: the free bid comes back.
	txRef := "R-" + uuid.New().String()[:12]
	if err := s.Chapa.CreateCheckoutPayment(ctx, chapapayment.CheckoutRequest{
		TxRef: txRef, UserID: user, AuctionID: next.ID, Amount: m(50), FreeBids: 1,
		BidValues: []domain.Money{m(11), m(12)},
	}); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, s, ids[1]); got != 0 {
		t.Fatalf("balance while pending = %d, want 0", got)
	}
	// A second checkout cannot spend the same free bid.
	err := s.Chapa.CreateCheckoutPayment(ctx, chapapayment.CheckoutRequest{
		TxRef: "R-" + uuid.New().String()[:12], UserID: user, AuctionID: next.ID, Amount: m(0), FreeBids: 1,
		BidValues: []domain.Money{m(13)},
	})
	if !errors.Is(err, domain.ErrFreeBidsUnavailable) {
		t.Fatalf("double spend err = %v, want ErrFreeBidsUnavailable", err)
	}
	if err := s.Chapa.ProcessVerified(ctx, &chapapayment.VerifiedSnapshot{TxRef: txRef, APIStatus: "success", DataStatus: "failed"}); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, s, ids[1]); got != 1 {
		t.Fatalf("balance after failed checkout = %d, want 1", got)
	}

	// One ticket fully covered by the free bid: issued without Chapa.
	freeRef := "R-" + uuid.New().String()[:12]
	if err := s.Chapa.CreateCheckoutPayment(ctx, chapapayment.CheckoutRequest{
		TxRef: freeRef, UserID: user, AuctionID: next.ID, Amount: domain.Zero(), FreeBids: 1,
		BidValues: []domain.Money{m(14)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Chapa.FulfillFreeCheckout(ctx, freeRef); err != nil {
		t.Fatalf("FulfillFreeCheckout: %v", err)
	}
	part, err := s.LUB.GetParticipation(next.ID, ids[1])
	if err != nil || part == nil || part.Status != domain.ParticipationStatusActive {
		t.Fatalf("participation = %+v, err %v", part, err)
	}
	if got := balance(t, s, ids[1]); got != 0 {
		t.Fatalf("balance after use = %d, want 0", got)
	}
}
