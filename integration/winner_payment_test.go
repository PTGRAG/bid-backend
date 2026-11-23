//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/testutil"
	"github.com/google/uuid"
)

// Pay only if you win: tickets are entrance fees only; after the draw the winner has 48 hours to
// pay their bid, with two reminders; unpaid prizes pass to the next lowest unique bid (3 offers max).

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func m(v float64) domain.Money { return domain.NewMoney(v) }

// drawnAuction seeds an active auction with the given bids (participant index -> values) and runs
// the draw. Participant 0 bids first, and so on.
func drawnAuction(t *testing.T, s *testutil.Stack, bids [][]float64) (*domain.Auction, []string) {
	t.Helper()
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive, EntranceFee: m(50)})
	ids := make([]string, len(bids))
	for i, vals := range bids {
		uid := testutil.SeedParticipant(t, s.PG, domain.Zero())
		ids[i] = uid.String()
		testutil.SeedAuctionParticipation(t, s.PG, a.ID, uid)
		for _, v := range vals {
			testutil.SeedBid(t, s.PG, a.ID, ids[i], m(v))
		}
	}
	if _, err := s.LUB.ExecuteDraw(context.Background(), a.ID, "test", uuid.Nil); err != nil {
		t.Fatalf("ExecuteDraw: %v", err)
	}
	got, err := s.LUB.GetAuctionByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got, ids
}

func openOffer(t *testing.T, s *testutil.Stack, auctionID uuid.UUID) *domain.WinnerOffer {
	t.Helper()
	o, err := s.WinnerOffers.GetOpen(auctionID)
	if err != nil {
		t.Fatal(err)
	}
	if o == nil {
		t.Fatal("expected an open winner offer")
	}
	return o
}

func countNotifications(t *testing.T, s *testutil.Stack, userID, notifType string) int64 {
	t.Helper()
	var n int64
	if err := s.PG.DB.Table("notifications").Where("user_id = ? AND type = ?", userID, notifType).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func hasAlert(s *testutil.Stack, substr string) bool {
	for _, msg := range s.Admin.Messages() {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

func TestWinner_DrawOpensOfferAndHidesUnpaidWinner(t *testing.T) {
	s := stack(t)
	start := time.Now().UTC()
	a, ids := drawnAuction(t, s, [][]float64{{3}, {7}, {7}})

	if a.Status != domain.AuctionStatusExecuted {
		t.Fatalf("status = %s, want executed", a.Status)
	}
	o := openOffer(t, s, a.ID)
	if o.ParticipantID != ids[0] || o.BidValue.String() != "3.00" || o.Attempt != 1 {
		t.Fatalf("offer = %+v", o)
	}
	window := o.DueAt.Sub(start)
	if window < domain.WinnerPaymentWindow-time.Minute || window > domain.WinnerPaymentWindow+time.Minute {
		t.Fatalf("due in %s, want 48h", window)
	}

	// Public view: winner not named until paid; status says awaiting payment.
	pub, _ := s.LUB.GetAuctionByID(a.ID)
	s.LUB.EnrichWinnerPayment(pub, "")
	if pub.WinnerPaymentStatus != domain.WinnerPaymentStatusAwaiting || pub.WinnerName != "" || pub.WinnerParticipantID != nil {
		t.Fatalf("public view leaks or wrong status: status=%q name=%q", pub.WinnerPaymentStatus, pub.WinnerName)
	}

	// Each viewer sees their own outcome.
	win, _ := s.LUB.GetAuctionByID(a.ID)
	s.LUB.EnrichWinnerPayment(win, ids[0])
	if win.MyResult == nil || win.MyResult.Outcome != domain.MyOutcomeWonPayNow || win.MyResult.DueAt == nil {
		t.Fatalf("winner result = %+v", win.MyResult)
	}
	lose, _ := s.LUB.GetAuctionByID(a.ID)
	s.LUB.EnrichWinnerPayment(lose, ids[1])
	if lose.MyResult == nil || lose.MyResult.Outcome != domain.MyOutcomeLost {
		t.Fatalf("loser result = %+v", lose.MyResult)
	}

	// Winner is told to pay; every other participant gets the in-app result.
	if n := countNotifications(t, s, ids[0], domain.NotificationTypeAuctionWinner); n != 1 {
		t.Fatalf("winner notifications = %d", n)
	}
	if n := countNotifications(t, s, ids[0], domain.NotificationTypeAuctionResult); n != 0 {
		t.Fatalf("winner must not get the loser message, got %d", n)
	}
	for _, id := range ids[1:] {
		if n := countNotifications(t, s, id, domain.NotificationTypeAuctionResult); n != 1 {
			t.Fatalf("loser %s result notifications = %d", id, n)
		}
	}
	if !hasAlert(s, "Winner drawn") {
		t.Fatalf("admin not alerted: %v", s.Admin.Messages())
	}
}

func TestWinner_TwoRemindersThenAdminCall(t *testing.T) {
	s := stack(t)
	a, ids := drawnAuction(t, s, [][]float64{{3}})
	o := openOffer(t, s, a.ID)
	ctx := context.Background()
	c := &clock{}
	s.LUB.SetClock(c.now)

	// 30h before the deadline: nothing yet.
	c.t = o.DueAt.Add(-30 * time.Hour)
	if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
		t.Fatal(err)
	}
	if got := openOffer(t, s, a.ID).RemindersSent; got != 0 {
		t.Fatalf("reminders at -30h = %d", got)
	}

	// 23h left: first reminder, once.
	c.t = o.DueAt.Add(-23 * time.Hour)
	for i := 0; i < 2; i++ {
		if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := openOffer(t, s, a.ID).RemindersSent; got != 1 {
		t.Fatalf("reminders at -23h = %d", got)
	}
	if hasAlert(s, "call them") {
		t.Fatal("admin should not be asked to call after the first reminder")
	}

	// 1h left: second reminder and the admin is asked to call.
	c.t = o.DueAt.Add(-1 * time.Hour)
	if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
		t.Fatal(err)
	}
	if got := openOffer(t, s, a.ID).RemindersSent; got != 2 {
		t.Fatalf("reminders at -1h = %d", got)
	}
	if n := countNotifications(t, s, ids[0], domain.NotificationTypeWinnerPaymentReminder); n != 2 {
		t.Fatalf("reminder notifications = %d, want 2", n)
	}
	if !hasAlert(s, "call them") || !hasAlert(s, "+251922000000") {
		t.Fatalf("admin call alert missing phone: %v", s.Admin.Messages())
	}
}

func TestWinner_UnpaidPassesToNextUniqueBidThenCancels(t *testing.T) {
	s := stack(t)
	// Unique values: 1 (A), 4 (D), 5 (E). 2 is tied between A and E and stays tied even after A
	// drops out: uniqueness is counted over every bid, as published in the draw results.
	a, ids := drawnAuction(t, s, [][]float64{
		{1, 2}, // A
		{4},    // D
		{2, 5}, // E
		{9, 8}, // F: 8 and 9 unique too, but higher
	})
	A, D, E := ids[0], ids[1], ids[2]
	ctx := context.Background()
	c := &clock{}
	s.LUB.SetClock(c.now)

	expire := func() {
		t.Helper()
		o := openOffer(t, s, a.ID)
		c.t = o.DueAt.Add(time.Minute)
		if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if o := openOffer(t, s, a.ID); o.ParticipantID != A || o.BidValue.String() != "1.00" {
		t.Fatalf("first offer = %s %s", o.ParticipantID, o.BidValue)
	}
	expire()
	o2 := openOffer(t, s, a.ID)
	if o2.ParticipantID != D || o2.BidValue.String() != "4.00" || o2.Attempt != 2 {
		t.Fatalf("second offer = %s %s attempt %d, want D 4.00 attempt 2", o2.ParticipantID, o2.BidValue, o2.Attempt)
	}
	got, _ := s.LUB.GetAuctionByID(a.ID)
	if got.WinnerParticipantID == nil || *got.WinnerParticipantID != D || got.WinnerBidValue.String() != "4.00" {
		t.Fatalf("auction winner not moved to D")
	}
	if n := countNotifications(t, s, A, domain.NotificationTypeWinnerPaymentExpired); n != 1 {
		t.Fatalf("A expired notifications = %d", n)
	}
	audit, err := s.LUB.GetAuctionAudit(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range audit.Entries {
		if e.IsWinner != (e.BidValue.String() == "4.00") {
			t.Fatalf("audit winner flag wrong on %s", e.BidValue)
		}
	}

	expire()
	o3 := openOffer(t, s, a.ID)
	if o3.ParticipantID != E || o3.BidValue.String() != "5.00" || o3.Attempt != 3 {
		t.Fatalf("third offer = %s %s attempt %d, want E 5.00 attempt 3", o3.ParticipantID, o3.BidValue, o3.Attempt)
	}

	expire()
	if o, _ := s.WinnerOffers.GetOpen(a.ID); o != nil {
		t.Fatalf("no offer should be open after 3 misses, got attempt %d", o.Attempt)
	}
	got, _ = s.LUB.GetAuctionByID(a.ID)
	if got.Status != domain.AuctionStatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	s.LUB.EnrichWinnerPayment(got, A)
	if got.WinnerPaymentStatus != domain.WinnerPaymentStatusUnpaid || got.MyResult.Outcome != domain.MyOutcomeWonExpired {
		t.Fatalf("after cancel: status=%q my=%+v", got.WinnerPaymentStatus, got.MyResult)
	}
	if !hasAlert(s, "no winner paid") {
		t.Fatalf("admin not told about cancellation: %v", s.Admin.Messages())
	}
}

func TestWinner_NoOtherUniqueBidCancels(t *testing.T) {
	s := stack(t)
	a, _ := drawnAuction(t, s, [][]float64{{1}, {6}, {6}})
	c := &clock{t: openOffer(t, s, a.ID).DueAt.Add(time.Minute)}
	s.LUB.SetClock(c.now)
	if err := s.LUB.ProcessWinnerPayments(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LUB.GetAuctionByID(a.ID)
	if got.Status != domain.AuctionStatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
}

// winnerCheckout records a pending winner checkout the way PayWinningBid does.
func winnerCheckout(t *testing.T, s *testutil.Stack, o *domain.WinnerOffer) string {
	t.Helper()
	txRef := "W-" + uuid.New().String()[:12]
	if err := s.Chapa.CreateWinnerPendingPayment(context.Background(), txRef, o); err != nil {
		t.Fatal(err)
	}
	return txRef
}

func chapaSuccess(txRef string, amount string) *chapapayment.VerifiedSnapshot {
	return &chapapayment.VerifiedSnapshot{TxRef: txRef, APIStatus: "success", DataStatus: "success", Amount: amount}
}

func TestWinner_PaysBidAndIsNamed(t *testing.T) {
	s := stack(t)
	a, ids := drawnAuction(t, s, [][]float64{{3}, {7}})
	o := openOffer(t, s, a.ID)
	ctx := context.Background()

	if _, err := s.LUB.RequirePayableOffer(a.ID, ids[1]); err == nil {
		t.Fatal("a loser must not be able to pay the winning bid")
	}
	if _, err := s.LUB.RequirePayableOffer(a.ID, ids[0]); err != nil {
		t.Fatalf("winner cannot pay: %v", err)
	}

	txRef := winnerCheckout(t, s, o)
	if err := s.Chapa.ProcessVerified(ctx, chapaSuccess(txRef, "3.00")); err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}
	paid, err := s.WinnerOffers.GetByID(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != domain.WinnerOfferPaid || paid.PaymentTxRef != txRef {
		t.Fatalf("offer after payment = %+v", paid)
	}
	p, _ := s.Ledger.GetByTxRef(ctx, txRef)
	if p.Status != domain.PaymentStatusSuccess || !p.Processed || p.Purpose != domain.PaymentPurposeWinner {
		t.Fatalf("ledger row = %+v", p)
	}
	// Webhook + return URL both arrive: second call is harmless.
	if err := s.Chapa.ProcessVerified(ctx, chapaSuccess(txRef, "3.00")); err != nil {
		t.Fatalf("ProcessVerified again: %v", err)
	}

	pub, _ := s.LUB.GetAuctionByID(a.ID)
	s.LUB.EnrichWinnerPayment(pub, "")
	if pub.WinnerPaymentStatus != domain.WinnerPaymentStatusPaid || pub.WinnerName == "" {
		t.Fatalf("paid winner should be public: status=%q name=%q", pub.WinnerPaymentStatus, pub.WinnerName)
	}
	if n := countNotifications(t, s, ids[0], domain.NotificationTypeWinnerPaid); n != 1 {
		t.Fatalf("paid notifications = %d", n)
	}
	if !hasAlert(s, "Arrange delivery") {
		t.Fatalf("admin not told to deliver: %v", s.Admin.Messages())
	}

	// Paid offers are never expired by the scheduler.
	c := &clock{t: o.DueAt.Add(time.Hour)}
	s.LUB.SetClock(c.now)
	if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LUB.GetAuctionByID(a.ID)
	if got.Status != domain.AuctionStatusExecuted {
		t.Fatalf("status = %s after deadline, want executed", got.Status)
	}
}

func TestWinner_ShortPaymentIsNotAccepted(t *testing.T) {
	s := stack(t)
	a, _ := drawnAuction(t, s, [][]float64{{30}})
	o := openOffer(t, s, a.ID)
	txRef := winnerCheckout(t, s, o)
	if err := s.Chapa.ProcessVerified(context.Background(), chapaSuccess(txRef, "3.00")); err != nil {
		t.Fatal(err)
	}
	if got := openOffer(t, s, a.ID); got.Status != domain.WinnerOfferAwaitingPayment {
		t.Fatalf("short payment must not settle the offer")
	}
	if !hasAlert(s, "too low") {
		t.Fatalf("admin not alerted: %v", s.Admin.Messages())
	}
}

func TestWinner_PendingPaymentGetsGraceThenLatePaymentFlagsRefund(t *testing.T) {
	s := stack(t)
	a, ids := drawnAuction(t, s, [][]float64{{3}, {4}})
	o := openOffer(t, s, a.ID)
	ctx := context.Background()
	c := &clock{}
	s.LUB.SetClock(c.now)

	txRef := winnerCheckout(t, s, o) // started before the deadline, still pending at Chapa

	c.t = o.DueAt.Add(10 * time.Minute)
	if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
		t.Fatal(err)
	}
	if got := openOffer(t, s, a.ID); got.ID != o.ID {
		t.Fatal("offer must stay open while its payment is being confirmed")
	}

	c.t = o.DueAt.Add(domain.WinnerPaymentGrace + time.Minute)
	if err := s.LUB.ProcessWinnerPayments(ctx); err != nil {
		t.Fatal(err)
	}
	next := openOffer(t, s, a.ID)
	if next.ParticipantID != ids[1] {
		t.Fatalf("prize should pass to the next bid after the grace period")
	}

	// The first winner's payment lands after all: it must not take the prize back.
	if err := s.Chapa.ProcessVerified(ctx, chapaSuccess(txRef, "3.00")); err != nil {
		t.Fatal(err)
	}
	if got := openOffer(t, s, a.ID); got.ID != next.ID {
		t.Fatal("late payment changed the open offer")
	}
	if !hasAlert(s, "Refund needed") {
		t.Fatalf("admin not told to refund: %v", s.Admin.Messages())
	}
}

func TestFreeTicket_CostsNothingAndSkipsChapa(t *testing.T) {
	s := stack(t)
	ctx := context.Background()
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status: domain.AuctionStatusActive, EntranceFee: m(50), FreeEntranceEnabled: true, FreeEntranceLimit: 10,
	})
	uid := testutil.SeedParticipant(t, s.PG, domain.Zero())

	charge := domain.CheckoutCharge([]domain.Money{m(12.5)}, a.EntranceFee, true)
	if !charge.IsZero() {
		t.Fatalf("one free ticket should cost 0, got %s", charge)
	}
	txRef := "R-" + uuid.New().String()[:12]
	if err := s.Chapa.CreateWaivedPendingPayment(ctx, txRef, uid, a.ID, charge, m(12.5)); err != nil {
		t.Fatalf("CreateWaivedPendingPayment: %v", err)
	}
	if err := s.Chapa.FulfillFreeCheckout(ctx, txRef); err != nil {
		t.Fatalf("FulfillFreeCheckout: %v", err)
	}
	part, err := s.LUB.GetParticipation(a.ID, uid.String())
	if err != nil || part == nil || !part.EntranceFree || !part.FeePaid.IsZero() {
		t.Fatalf("participation = %+v err=%v", part, err)
	}
	bids, _ := s.LUB.ListMyBids(a.ID, uid.String())
	if len(bids) != 1 || bids[0].BidValue.String() != "12.50" {
		t.Fatalf("bids = %+v", bids)
	}
	p, _ := s.Ledger.GetByTxRef(ctx, txRef)
	if p.Status != domain.PaymentStatusSuccess || !p.Processed || !p.EntranceWaived {
		t.Fatalf("ledger row = %+v", p)
	}
	// A second free ticket is never possible for the same customer.
	a2 := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status: domain.AuctionStatusActive, EntranceFee: m(50), FreeEntranceEnabled: true, FreeEntranceLimit: 10,
	})
	if err := s.Chapa.CreateWaivedPendingPayment(ctx, "R-"+uuid.New().String()[:12], uid, a2.ID, domain.Zero(), m(3)); err == nil {
		t.Fatal("second free ticket must be refused")
	}
}
