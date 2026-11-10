//go:build integration

package testutil

import (
	"log/slog"
	"testing"

	"github.com/bid-hub/admin-backend/adapters/repository/postgres"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/core/services/notifications"
)

type Stack struct {
	PG    *PG
	Audit *services.AuditLogService
	LUB   *lub.Service
	Chapa *chapapayment.Service
	Notif *notifications.Service
	// Ledger is the payments ledger repository (free-entrance reservations live here).
	Ledger ports.PaymentLedgerRepository
	// Auctions is the auction repository.
	Auctions ports.AuctionRepository
}

func NewStack(t testing.TB, pg *PG) *Stack {
	t.Helper()
	db := pg.DB

	userRepo := postgres.NewUserRepository(db)
	participantUserRepo := postgres.NewParticipantUserRepository(db)
	auctionRepo := postgres.NewAuctionRepository(db)
	participationRepo := postgres.NewAuctionParticipationRepository(db)
	bidRepo := postgres.NewBidRepository(db)
	notificationRepo := postgres.NewNotificationRepository(db)
	paymentLedgerRepo := postgres.NewPaymentLedgerRepository(db)
	auditRepo := postgres.NewAuditLogRepository(db)

	auditSvc := services.NewAuditLogService(auditRepo)
	notifSvc := notifications.NewService(notificationRepo, userRepo, participantUserRepo, participationRepo, nil, nil, nil)

	lubSvc := lub.NewService(
		auctionRepo,
		participationRepo,
		bidRepo,
		auditSvc,
		notifSvc,
		nil,
	)

	chapaSvc := chapapayment.NewService(
		paymentLedgerRepo, lubSvc, participantUserRepo, notifSvc, slog.Default(),
	)

	return &Stack{
		PG:    pg,
		Audit: auditSvc,
		LUB:   lubSvc,
		Chapa: chapaSvc,
		Notif: notifSvc,

		Ledger:   paymentLedgerRepo,
		Auctions: auctionRepo,
	}
}
