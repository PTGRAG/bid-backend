package lub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

type mockAuctionRepo struct {
	getFn      func(id uuid.UUID) (*domain.Auction, error)
	updateFn   func(a *domain.Auction) error
	updateStFn func(id uuid.UUID, s domain.AuctionStatus) error
	listDueFn  func() ([]domain.Auction, error)
	createFn   func(a *domain.Auction) error
}

func (m *mockAuctionRepo) Create(a *domain.Auction) error {
	if m.createFn != nil {
		return m.createFn(a)
	}
	return nil
}
func (m *mockAuctionRepo) GetByID(id uuid.UUID) (*domain.Auction, error) {
	if m.getFn != nil {
		return m.getFn(id)
	}
	return nil, errors.New("not implemented")
}
func (m *mockAuctionRepo) Update(a *domain.Auction) error {
	if m.updateFn != nil {
		return m.updateFn(a)
	}
	return nil
}
func (m *mockAuctionRepo) UpdateStatus(id uuid.UUID, s domain.AuctionStatus) error {
	if m.updateStFn != nil {
		return m.updateStFn(id, s)
	}
	return nil
}
func (m *mockAuctionRepo) List(status, search string, l, o int) ([]domain.Auction, int64, error) {
	return nil, 0, nil
}
func (m *mockAuctionRepo) ListPublic(l, o int) ([]domain.Auction, int64, error) {
	return nil, 0, nil
}
func (m *mockAuctionRepo) ListPublicFiltered(filter ports.PublicAuctionFilter) ([]domain.Auction, int64, map[string]int64, error) {
	return nil, 0, nil, nil
}
func (m *mockAuctionRepo) ListDueForClose() ([]domain.Auction, error) {
	if m.listDueFn != nil {
		return m.listDueFn()
	}
	return nil, nil
}
func (m *mockAuctionRepo) Delete(id uuid.UUID) error { return nil }

type mockPartRepo struct {
	getFn          func(auctionID uuid.UUID, pid string) (*domain.AuctionParticipation, error)
	getByTxFn      func(tx string) (*domain.AuctionParticipation, error)
	createFn       func(p *domain.AuctionParticipation) error
	listByAuctFn   func(auctionID uuid.UUID, l, o int) ([]domain.AuctionParticipation, int64, error)
	updateStatusFn func(id uuid.UUID, s string) error
	countFn        func(auctionID uuid.UUID) (int64, error)
	hasUsedFreeFn  func(pid string) (bool, error)
}

func (m *mockPartRepo) Create(p *domain.AuctionParticipation) error {
	if m.createFn != nil {
		return m.createFn(p)
	}
	return nil
}
func (m *mockPartRepo) GetByAuctionAndParticipant(auctionID uuid.UUID, pid string) (*domain.AuctionParticipation, error) {
	if m.getFn != nil {
		return m.getFn(auctionID, pid)
	}
	return nil, nil
}
func (m *mockPartRepo) GetByTransactionID(tx string) (*domain.AuctionParticipation, error) {
	if m.getByTxFn != nil {
		return m.getByTxFn(tx)
	}
	return nil, nil
}
func (m *mockPartRepo) ListByAuction(auctionID uuid.UUID, l, o int) ([]domain.AuctionParticipation, int64, error) {
	if m.listByAuctFn != nil {
		return m.listByAuctFn(auctionID, l, o)
	}
	return nil, 0, nil
}
func (m *mockPartRepo) ListByParticipant(participantID string, l, o int) ([]domain.AuctionParticipation, int64, error) {
	return nil, 0, nil
}
func (m *mockPartRepo) UpdateStatus(id uuid.UUID, s string) error {
	if m.updateStatusFn != nil {
		return m.updateStatusFn(id, s)
	}
	return nil
}
func (m *mockPartRepo) CountByAuction(auctionID uuid.UUID) (int64, error) {
	if m.countFn != nil {
		return m.countFn(auctionID)
	}
	return 0, nil
}
func (m *mockPartRepo) CountByAuctionSince(auctionID uuid.UUID, since time.Time) (int64, error) {
	return 0, nil
}
func (m *mockPartRepo) CountByAuctions(auctionIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	return nil, nil
}
func (m *mockPartRepo) CountByAuctionsSince(auctionIDs []uuid.UUID, since time.Time) (map[uuid.UUID]int64, error) {
	return nil, nil
}
func (m *mockPartRepo) GetCountByMonth(lastMonths int) ([]domain.MonthlyCount, error) {
	return nil, nil
}

func (m *mockPartRepo) JoinedAuctionSet(auctionIDs []uuid.UUID, participantID string) (map[uuid.UUID]bool, error) {
	return nil, nil
}
func (m *mockPartRepo) HasUsedFreeEntrance(participantID string) (bool, error) {
	if m.hasUsedFreeFn != nil {
		return m.hasUsedFreeFn(participantID)
	}
	return false, nil
}

type mockBidRepo struct {
	getFn        func(id uuid.UUID) (*domain.Bid, error)
	createFn     func(b *domain.Bid) error
	updateFn     func(b *domain.Bid) error
	findWinnerFn func(auctionID uuid.UUID) (*domain.Bid, error)
	markWinnerFn func(auctionID, winner uuid.UUID) error
	getByAVFn    func(aid uuid.UUID, participant string, v domain.Money) (*domain.Bid, error)
	listByPFn    func(aid uuid.UUID, participant string) ([]domain.Bid, error)
	listByAuctFn func(aid uuid.UUID, l, o int) ([]domain.Bid, int64, error)
}

func (m *mockBidRepo) Create(b *domain.Bid) error {
	if m.createFn != nil {
		return m.createFn(b)
	}
	return nil
}
func (m *mockBidRepo) GetByID(id uuid.UUID) (*domain.Bid, error) {
	if m.getFn != nil {
		return m.getFn(id)
	}
	return nil, errors.New("not found")
}
func (m *mockBidRepo) Update(b *domain.Bid) error {
	if m.updateFn != nil {
		return m.updateFn(b)
	}
	return nil
}
func (m *mockBidRepo) GetByAuctionAndParticipantAndValue(aid uuid.UUID, p string, v domain.Money) (*domain.Bid, error) {
	if m.getByAVFn != nil {
		return m.getByAVFn(aid, p, v)
	}
	return nil, nil
}
func (m *mockBidRepo) ListByAuctionAndParticipant(aid uuid.UUID, p string) ([]domain.Bid, error) {
	if m.listByPFn != nil {
		return m.listByPFn(aid, p)
	}
	return nil, nil
}
func (m *mockBidRepo) ListByAuction(aid uuid.UUID, l, o int) ([]domain.Bid, int64, error) {
	if m.listByAuctFn != nil {
		return m.listByAuctFn(aid, l, o)
	}
	return nil, 0, nil
}
func (m *mockBidRepo) FindWinner(aid uuid.UUID) (*domain.Bid, error) {
	if m.findWinnerFn != nil {
		return m.findWinnerFn(aid)
	}
	return nil, nil
}
func (m *mockBidRepo) MarkWinner(aid, winner uuid.UUID) error {
	if m.markWinnerFn != nil {
		return m.markWinnerFn(aid, winner)
	}
	return nil
}
func (m *mockBidRepo) DeleteBid(id uuid.UUID) error { return nil }

func (m *mockBidRepo) GetCountByMonth(lastMonths int) ([]domain.MonthlyCount, error) {
	return nil, nil
}

type mockAudit struct{}

func (mockAudit) Log(log *domain.AuditLog, ctx *domain.AuditContext) error { return nil }

func newSvc(ar *mockAuctionRepo, pa *mockPartRepo, br *mockBidRepo) *Service {
	return NewService(ar, pa, br, mockAudit{}, nil, nil)
}

func TestExecuteDraw_AlreadyExecuted(t *testing.T) {
	aid := uuid.New()
	ar := &mockAuctionRepo{getFn: func(id uuid.UUID) (*domain.Auction, error) {
		return &domain.Auction{ID: id, Status: domain.AuctionStatusExecuted}, nil
	}}
	svc := newSvc(ar, &mockPartRepo{}, &mockBidRepo{})
	_, err := svc.ExecuteDraw(context.Background(), aid, "manual", uuid.Nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestExecuteDraw_Cancelled(t *testing.T) {
	aid := uuid.New()
	ar := &mockAuctionRepo{getFn: func(id uuid.UUID) (*domain.Auction, error) {
		return &domain.Auction{ID: id, Status: domain.AuctionStatusCancelled}, nil
	}}
	svc := newSvc(ar, &mockPartRepo{}, &mockBidRepo{})
	_, err := svc.ExecuteDraw(context.Background(), aid, "manual", uuid.Nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestExecuteDraw_NoWinner(t *testing.T) {
	aid := uuid.New()
	ar := &mockAuctionRepo{
		getFn: func(id uuid.UUID) (*domain.Auction, error) {
			return &domain.Auction{ID: id, Status: domain.AuctionStatusActive}, nil
		},
		updateStFn: func(id uuid.UUID, s domain.AuctionStatus) error { return nil },
	}
	br := &mockBidRepo{findWinnerFn: func(aid uuid.UUID) (*domain.Bid, error) { return nil, nil }}
	svc := newSvc(ar, &mockPartRepo{}, br)
	res, err := svc.ExecuteDraw(context.Background(), aid, "manual", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.HasWinner {
		t.Fatal("expected no winner")
	}
}

func TestExecuteDraw_WithWinner(t *testing.T) {
	aid := uuid.New()
	participant := "user-1"
	closeAt := time.Now().Add(time.Hour)
	ar := &mockAuctionRepo{
		getFn: func(id uuid.UUID) (*domain.Auction, error) {
			return &domain.Auction{ID: id, Status: domain.AuctionStatusActive, CloseAt: &closeAt}, nil
		},
		updateStFn: func(id uuid.UUID, s domain.AuctionStatus) error { return nil },
		updateFn:   func(a *domain.Auction) error { return nil },
	}
	bid := &domain.Bid{ID: uuid.New(), AuctionID: aid, ParticipantID: participant, BidValue: domain.NewMoney(5)}
	br := &mockBidRepo{
		findWinnerFn: func(a uuid.UUID) (*domain.Bid, error) { return bid, nil },
		markWinnerFn: func(a, w uuid.UUID) error { return nil },
	}
	svc := newSvc(ar, &mockPartRepo{}, br)
	res, err := svc.ExecuteDraw(context.Background(), aid, "manual", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasWinner {
		t.Fatal("expected winner")
	}
	if res.WinnerBidID != bid.ID {
		t.Fatalf("winner mismatch")
	}
}

func TestCreateAuction_Defaults(t *testing.T) {
	ar := &mockAuctionRepo{createFn: func(a *domain.Auction) error { return nil }}
	svc := newSvc(ar, &mockPartRepo{}, &mockBidRepo{})
	a := &domain.Auction{Title: "x"}
	actor := uuid.New()
	if err := svc.CreateAuction(context.Background(), a, actor); err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AuctionStatusDraft {
		t.Fatalf("status=%s", a.Status)
	}
	if a.MinBidValue.Float64() != 0.01 {
		t.Fatalf("minbid=%v", a.MinBidValue.Float64())
	}
}

func TestActivateAuction_MissingFields(t *testing.T) {
	ar := &mockAuctionRepo{getFn: func(id uuid.UUID) (*domain.Auction, error) {
		return &domain.Auction{ID: id, Status: domain.AuctionStatusDraft}, nil
	}}
	svc := newSvc(ar, &mockPartRepo{}, &mockBidRepo{})
	if err := svc.ActivateAuction(context.Background(), uuid.New(), uuid.New()); err == nil {
		t.Fatal("expected err")
	}
}

func TestPlaceBid_RequiresParticipation(t *testing.T) {
	closeAt := time.Now().Add(time.Hour)
	ar := &mockAuctionRepo{getFn: func(id uuid.UUID) (*domain.Auction, error) {
		return &domain.Auction{ID: id, Status: domain.AuctionStatusActive, CloseAt: &closeAt, MinBidValue: domain.NewMoney(1)}, nil
	}}
	pa := &mockPartRepo{getFn: func(aid uuid.UUID, participant string) (*domain.AuctionParticipation, error) {
		return nil, nil
	}}
	br := &mockBidRepo{}
	svc := newSvc(ar, pa, br)
	_, err := svc.PlaceBid(context.Background(), uuid.New(), "u1", domain.NewMoney(5))
	if err == nil {
		t.Fatal("expected participation error")
	}
}

func TestPlaceBid_Success(t *testing.T) {
	closeAt := time.Now().Add(time.Hour)
	ar := &mockAuctionRepo{getFn: func(id uuid.UUID) (*domain.Auction, error) {
		return &domain.Auction{ID: id, Status: domain.AuctionStatusActive, CloseAt: &closeAt, MinBidValue: domain.NewMoney(1)}, nil
	}}
	pa := &mockPartRepo{getFn: func(aid uuid.UUID, participant string) (*domain.AuctionParticipation, error) {
		return &domain.AuctionParticipation{Status: domain.ParticipationStatusActive}, nil
	}}
	br := &mockBidRepo{}
	svc := newSvc(ar, pa, br)
	b, err := svc.PlaceBid(context.Background(), uuid.New(), "u1", domain.NewMoney(5))
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("nil bid")
	}
}
