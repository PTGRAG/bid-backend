//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/testutil"
	"github.com/google/uuid"
)

var pg *testutil.PG

func TestMain(m *testing.M) {
	code := m.Run()
	if pg != nil {
		pg.Stop()
	}
	os.Exit(code)
}

func stack(t *testing.T) *testutil.Stack {
	t.Helper()
	if pg == nil {
		pg = testutil.MustStartPostgres(t)
	}
	if pg == nil {
		t.SkipNow()
	}
	pg.Reset(t)
	return testutil.NewStack(t, pg)
}

func chapaSnap(txRef string, auctionID, userID uuid.UUID, amount string) *chapapayment.VerifiedSnapshot {
	return &chapapayment.VerifiedSnapshot{
		TxRef:         txRef,
		APIStatus:     "success",
		DataStatus:    "success",
		Amount:        amount,
		CustomerEmail: "buyer@test.local",
		CustomerFirst: "Buyer",
		Meta: map[string]interface{}{
			"auction_id":   auctionID.String(),
			"user_id":      userID.String(),
			"entrance_fee": amount,
		},
	}
}

func TestChapaWebhook_HappyPath_CreatesParticipationAndMarksPaymentProcessed(t *testing.T) {
	s := stack(t)
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status:      domain.AuctionStatusActive,
		EntranceFee: domain.NewMoney(50),
		StartInPast: true,
	})
	userID := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx := "TXREF-HAPPY-" + uuid.New().String()[:8]

	err := s.Chapa.ProcessVerified(context.Background(), chapaSnap(tx, a.ID, userID, "50"))
	if err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}

	part, err := s.LUB.GetParticipation(a.ID, userID.String())
	if err != nil {
		t.Fatalf("GetParticipation: %v", err)
	}
	if part == nil {
		t.Fatal("participation should exist")
	}
	if part.AuctionID != a.ID || part.ParticipantID != userID.String() {
		t.Fatalf("participation bound to wrong ids: %+v", part)
	}
	if part.FeePaid.String() != "50.00" {
		t.Errorf("fee paid: want 50.00, got %s", part.FeePaid.String())
	}

	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatalf("payment ledger row missing: %v", err)
	}
	if p.Status != domain.PaymentStatusSuccess {
		t.Errorf("ledger status: want success, got %s", p.Status)
	}
	if !p.Processed {
		t.Errorf("ledger.processed should be true after participation creation")
	}
}

func TestChapaWebhook_DuplicateDelivery_IsIdempotent(t *testing.T) {
	s := stack(t)
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status:      domain.AuctionStatusActive,
		EntranceFee: domain.NewMoney(10),
		StartInPast: true,
	})
	userID := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx := "TXREF-DUPE-" + uuid.New().String()[:8]
	snap := chapaSnap(tx, a.ID, userID, "10")

	if err := s.Chapa.ProcessVerified(context.Background(), snap); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := s.Chapa.ProcessVerified(context.Background(), snap); err != nil {
		t.Fatalf("second delivery should be idempotent, got: %v", err)
	}

	var count int64
	if err := s.PG.DB.Model(&domain.AuctionParticipation{}).Where("transaction_id = ?", tx).Count(&count).Error; err != nil {
		t.Fatalf("count participations: %v", err)
	}
	if count != 1 {
		t.Errorf("duplicate delivery produced %d participations (want 1)", count)
	}
}

func TestChapaWebhook_VerifyStatusNotSuccess_RecordsFailureNoParticipation(t *testing.T) {
	s := stack(t)
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status:      domain.AuctionStatusActive,
		EntranceFee: domain.NewMoney(10),
		StartInPast: true,
	})
	userID := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx := "TXREF-DECLINED-" + uuid.New().String()[:8]
	snap := chapaSnap(tx, a.ID, userID, "10")
	snap.DataStatus = "failed"

	if err := s.Chapa.ProcessVerified(context.Background(), snap); err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}

	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatalf("payment row missing: %v", err)
	}
	if p.Status != domain.PaymentStatusFailed {
		t.Errorf("ledger status: want failed, got %s", p.Status)
	}

	part, _ := s.LUB.GetParticipation(a.ID, userID.String())
	if part != nil {
		t.Errorf("participation was created for a declined payment")
	}
}

// Chapa answers "pending" while the customer is still on the checkout page (and right after
// the return redirect). That must not mark the payment failed or send a "payment failed" SMS.
func TestChapaVerify_PendingIsNotAFailure_AndLaterSuccessStillFulfils(t *testing.T) {
	s := stack(t)
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status:      domain.AuctionStatusActive,
		EntranceFee: domain.NewMoney(10),
		StartInPast: true,
	})
	userID := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx := "TXREF-PENDING-" + uuid.New().String()[:8]
	if err := s.Chapa.CreatePendingPayment(context.Background(), tx, userID, a.ID, domain.NewMoney(10)); err != nil {
		t.Fatalf("pending payment: %v", err)
	}

	pending := chapaSnap(tx, a.ID, userID, "10")
	pending.DataStatus = "pending"
	if err := s.Chapa.ProcessVerified(context.Background(), pending); err != nil {
		t.Fatalf("ProcessVerified(pending): %v", err)
	}
	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	if p.Status != domain.PaymentStatusPending || p.Processed {
		t.Fatalf("pending verify must leave the row pending/unprocessed, got status=%s processed=%v", p.Status, p.Processed)
	}
	if part, _ := s.LUB.GetParticipation(a.ID, userID.String()); part != nil {
		t.Fatalf("no participation while pending")
	}

	paid := chapaSnap(tx, a.ID, userID, "10")
	if err := s.Chapa.ProcessVerified(context.Background(), paid); err != nil {
		t.Fatalf("ProcessVerified(success): %v", err)
	}
	if part, _ := s.LUB.GetParticipation(a.ID, userID.String()); part == nil {
		t.Fatalf("later success must fulfil the participation")
	}
}

func TestChapaWebhook_MetaMissing_RecordsFailureNoParticipation(t *testing.T) {
	s := stack(t)
	tx := "TXREF-NOMETA-" + uuid.New().String()[:8]
	snap := &chapapayment.VerifiedSnapshot{
		TxRef:      tx,
		APIStatus:  "success",
		DataStatus: "success",
		Amount:     "10",
		Meta:       nil,
	}

	if err := s.Chapa.ProcessVerified(context.Background(), snap); err != nil {
		t.Fatalf("ProcessVerified should not return error for invalid meta (failure is recorded), got: %v", err)
	}

	var p domain.Payment
	if err := s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error; err != nil {
		t.Fatalf("payment row missing: %v", err)
	}
	if p.Status != domain.PaymentStatusFailed {
		t.Errorf("ledger status: want failed, got %s", p.Status)
	}
	if p.LastError == "" {
		t.Errorf("ledger.last_error should describe the meta parse failure")
	}
}

func TestChapaRetry_ParticipationExists_MarksProcessed(t *testing.T) {
	s := stack(t)
	a := testutil.SeedAuction(t, s.PG, testutil.AuctionOpts{
		Status:      domain.AuctionStatusActive,
		EntranceFee: domain.NewMoney(10),
		StartInPast: true,
	})
	userID := testutil.SeedParticipant(t, s.PG, domain.Zero())
	tx := "TXREF-RECONCILE-" + uuid.New().String()[:8]

	if err := s.Chapa.ProcessVerified(context.Background(), chapaSnap(tx, a.ID, userID, "10")); err != nil {
		t.Fatalf("ProcessVerified: %v", err)
	}

	if err := s.PG.DB.Model(&domain.Payment{}).Where("tx_ref = ?", tx).Updates(map[string]interface{}{
		"processed": false,
	}).Error; err != nil {
		t.Fatalf("reset processed: %v", err)
	}

	done, err := s.Chapa.RetryUnprocessedPayments(context.Background(), 10)
	if err != nil {
		t.Fatalf("RetryUnprocessedPayments: %v", err)
	}
	if done != 1 {
		t.Errorf("expected 1 reconciled row, got %d", done)
	}
	var p domain.Payment
	_ = s.PG.DB.Where("tx_ref = ?", tx).First(&p).Error
	if !p.Processed {
		t.Errorf("ledger should be marked processed after participation reconcile")
	}
}
