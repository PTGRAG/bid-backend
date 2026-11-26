//go:build integration

package testutil

import (
	"context"
	"log/slog"
	"sync"
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
	// WinnerOffers is the pay-only-if-you-win offers repository.
	WinnerOffers ports.WinnerOfferRepository
	// FreeBids is the near-miss free-bid repository.
	FreeBids ports.FreeBidRepository
	// Admin records staff alerts (Telegram admin chat in production).
	Admin *AdminRecorder
}

// AdminRecorder is an in-memory ports.AdminNotifier.
type AdminRecorder struct {
	mu   sync.Mutex
	Msgs []string
}

func (a *AdminRecorder) NotifyAdmin(_ context.Context, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Msgs = append(a.Msgs, text)
}

// Messages returns a copy of the alerts sent so far.
func (a *AdminRecorder) Messages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.Msgs...)
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

	admin := &AdminRecorder{}
	winnerOffers := postgres.NewWinnerOfferRepository(db)
	lubSvc.SetWinnerOffers(winnerOffers)
	lubSvc.SetAdminNotifier(admin)
	lubSvc.SetWinnerPaymentChecker(chapaSvc)
	lubSvc.SetContactLookup(notifSvc)
	freeBids := postgres.NewFreeBidRepository(db)
	lubSvc.SetFreeBids(freeBids)
	chapaSvc.SetAdminNotifier(admin)

	return &Stack{
		PG:    pg,
		Audit: auditSvc,
		LUB:   lubSvc,
		Chapa: chapaSvc,
		Notif: notifSvc,

		Ledger:       paymentLedgerRepo,
		Auctions:     auctionRepo,
		WinnerOffers: winnerOffers,
		FreeBids:     freeBids,
		Admin:        admin,
	}
}
