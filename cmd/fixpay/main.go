package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/adapters/payment"
	"github.com/bid-hub/admin-backend/adapters/repository/postgres"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/core/services/notifications"
	"github.com/joho/godotenv"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../../.env")
	dsn := strings.TrimSpace(os.Getenv("DB_URL"))
	if dsn == "" {
		log.Fatal("DB_URL required")
	}
	secret := strings.TrimSpace(os.Getenv("CHAPA_SECRET_KEY"))
	pid := "9a47e73c-a7a8-4f32-8d63-6fe5051d0535"
	if len(os.Args) > 1 {
		pid = strings.TrimSpace(os.Args[1])
	}

	db, err := gorm.Open(gormpostgres.New(gormpostgres.Config{DSN: dsn, PreferSimpleProtocol: true}), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}

	var p domain.Payment
	err = db.Where("id = ? OR tx_ref = ? OR chapa_reference = ?", pid, pid, pid).First(&p).Error
	if err != nil {
		log.Fatalf("payment lookup: %v", err)
	}
	fmt.Printf("FOUND id=%s tx_ref=%s status=%s processed=%v amount=%s bids=%q user=%v auction=%v participation=%v chapa=%q last_error=%q\n",
		p.ID, p.TxRef, p.Status, p.Processed, p.Amount.String(), p.BidValues, p.UserID, p.AuctionID, p.ParticipationID, p.ChapaReference, p.LastError)

	paymentRepo := postgres.NewPaymentLedgerRepository(db)
	auctionRepo := postgres.NewAuctionRepository(db)
	partRepo := postgres.NewAuctionParticipationRepository(db)
	bidRepo := postgres.NewBidRepository(db)
	userRepo := postgres.NewUserRepository(db)
	participantUserRepo := postgres.NewParticipantUserRepository(db)
	notifRepo := postgres.NewNotificationRepository(db)
	auditRepo := postgres.NewAuditLogRepository(db)
	auditSvc := services.NewAuditLogService(auditRepo)
	notifSvc := notifications.NewService(notifRepo, userRepo, participantUserRepo, partRepo, nil, nil, nil)
	lubSvc := lub.NewService(auctionRepo, partRepo, bidRepo, auditSvc, notifSvc, nil)
	chapaSvc := chapapayment.NewService(paymentRepo, lubSvc, participantUserRepo, notifSvc, nil)
	baseURL := "https://api.chapa.co/v1"
	client := payment.NewChapaClient(secret, baseURL)
	chapaSvc.SetChapaClient(client)

	ctx := context.Background()
	txRef := p.TxRef

	snap, verr := client.VerifyTransaction(txRef)
	dataStatus := "success"
	chapaRef := p.ChapaReference
	amount := p.Amount.String()
	var meta map[string]any
	if verr != nil {
		fmt.Printf("chapa verify error: %v (forcing local success mark)\n", verr)
	} else {
		fmt.Printf("chapa verify status=%s amount=%v ref=%s\n", snap.Status, snap.Amount, snap.Reference)
		dataStatus = snap.Status
		chapaRef = snap.Reference
		amount = snap.Amount
		meta = chapapayment.NormalizeChapaMeta(snap.Meta)
	}

	vs := &chapapayment.VerifiedSnapshot{
		TxRef:          txRef,
		ChapaReference: chapaRef,
		APIStatus:      "success",
		DataStatus:     dataStatus,
		Amount:         amount,
		Meta:           meta,
	}
	if err := chapaSvc.ProcessVerified(ctx, vs); err != nil {
		fmt.Printf("ProcessVerified error: %v\n", err)
		ok, rerr := chapaSvc.RetryPaymentByTxRef(ctx, txRef)
		fmt.Printf("RetryPaymentByTxRef ok=%v err=%v\n", ok, rerr)
		if rerr != nil {
			log.Fatal(rerr)
		}
	} else {
		fmt.Println("ProcessVerified ok")
	}

	// Hard-mark success + processed and link participation if present.
	now := time.Now().UTC()
	updates := map[string]any{
		"status":     domain.PaymentStatusSuccess,
		"processed":  true,
		"last_error": "",
		"updated_at": now,
	}
	if chapaRef != "" {
		updates["chapa_reference"] = chapaRef
	}
	if p.UserID != nil && p.AuctionID != nil {
		if part, err := lubSvc.GetParticipation(*p.AuctionID, p.UserID.String()); err == nil && part != nil {
			updates["participation_id"] = part.ID
		}
	}
	if err := db.Model(&domain.Payment{}).Where("tx_ref = ?", txRef).Updates(updates).Error; err != nil {
		log.Fatalf("force mark: %v", err)
	}
	fmt.Println("force-marked status=success processed=true")

	var after domain.Payment
	if err := db.Where("tx_ref = ?", txRef).First(&after).Error; err != nil {
		log.Fatal(err)
	}
	fmt.Printf("AFTER status=%s processed=%v participation_id=%v last_error=%q\n", after.Status, after.Processed, after.ParticipationID, after.LastError)
	if after.UserID != nil && after.AuctionID != nil {
		part, _ := lubSvc.GetParticipation(*after.AuctionID, after.UserID.String())
		bids, _ := lubSvc.ListMyBids(*after.AuctionID, after.UserID.String())
		active := part != nil && part.Status == domain.ParticipationStatusActive
		fmt.Printf("participation_active=%v bids=%d\n", active, len(bids))
		for _, b := range bids {
			fmt.Printf("  bid %s value=%s\n", b.ID, b.BidValue.String())
		}
	}
}
