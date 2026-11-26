package referral

import (
	"context"
	"errors"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

// Dashboard data for agents (their own codes) and staff (any code). The agent dashboard UI
// reads these shapes; keep them stable.

type AuctionRef struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
}

type ProgramView struct {
	ID                    uuid.UUID             `json:"id"`
	Name                  string                `json:"name"`
	Kind                  string                `json:"kind"`
	IsDefault             bool                  `json:"isDefault"`
	Threshold             int                   `json:"threshold"`
	Tiers                 []domain.ReferralTier `json:"tiers"`
	AttributionWindowDays int                   `json:"attributionWindowDays"`
	// QualifyingAuctions lists the auctions a paid ticket must be on; empty means any auction.
	QualifyingAuctions []AuctionRef `json:"qualifyingAuctions"`
}

type CodeView struct {
	ID        uuid.UUID `json:"id"`
	Code      string    `json:"code"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	Status    string    `json:"status"`
	Link      string    `json:"link"`
	CreatedAt time.Time `json:"createdAt"`
}

// Funnel is link click -> signup -> qualified bidder for one code (or a sum of codes).
type Funnel struct {
	Visitors  int64 `json:"visitors"`
	Visits    int64 `json:"visits"`
	Signups   int64 `json:"signups"`
	Pending   int64 `json:"pending"`
	Qualified int64 `json:"qualified"`
	Rejected  int64 `json:"rejected"`
	// Percentages, 0-100, one decimal; 0 when the base is 0.
	VisitorToSignupRate   float64 `json:"visitorToSignupRate"`
	SignupToQualifiedRate float64 `json:"signupToQualifiedRate"`
}

type EarningsView struct {
	domain.ReferralEarnings
	PaidOut domain.Money `json:"paidOut"`
	// Owed is earned minus paid out (never negative); Overpaid is the excess when a refund took
	// a qualified bidder away after a payout.
	Owed     domain.Money `json:"owed"`
	Overpaid domain.Money `json:"overpaid"`
}

type CodeSummary struct {
	Code     CodeView     `json:"code"`
	Program  ProgramView  `json:"program"`
	Funnel   Funnel       `json:"funnel"`
	Earnings EarningsView `json:"earnings"`
}

type Totals struct {
	Funnel  Funnel       `json:"funnel"`
	Earned  domain.Money `json:"earned"`
	PaidOut domain.Money `json:"paidOut"`
	Owed    domain.Money `json:"owed"`
}

// AgentDashboard is GET /referrals/me.
type AgentDashboard struct {
	IsReferrer bool          `json:"isReferrer"` // false when the account owns no code
	Codes      []CodeSummary `json:"codes"`
	Totals     Totals        `json:"totals"`
}

func pct(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return float64(int64(float64(n)*1000/float64(d)+0.5)) / 10
}

func (s *Service) link(code string) string {
	base := s.appURL
	if base == "" {
		base = "https://carramarket.com"
	}
	return base + "/?ref=" + code
}

func (s *Service) programView(p *domain.ReferralProgram) (ProgramView, error) {
	v := ProgramView{
		ID: p.ID, Name: p.Name, Kind: p.Kind, IsDefault: p.IsDefault, Threshold: p.Threshold,
		Tiers: p.Tiers, AttributionWindowDays: p.AttributionWindowDays, QualifyingAuctions: []AuctionRef{},
	}
	if v.Tiers == nil {
		v.Tiers = []domain.ReferralTier{}
	}
	titles, err := s.repo.AuctionTitles(p.QualifyingAuctionIDs)
	if err != nil {
		return v, err
	}
	for _, id := range p.QualifyingAuctionIDs {
		v.QualifyingAuctions = append(v.QualifyingAuctions, AuctionRef{ID: id, Title: titles[id]})
	}
	return v, nil
}

func (s *Service) summarize(codes []domain.ReferralCode) ([]CodeSummary, Totals, error) {
	totals := Totals{Earned: domain.Zero(), PaidOut: domain.Zero(), Owed: domain.Zero()}
	ids := make([]uuid.UUID, len(codes))
	for i := range codes {
		ids[i] = codes[i].ID
	}
	counts, err := s.repo.Counts(ids)
	if err != nil {
		return nil, totals, err
	}
	programs := map[string]*domain.ReferralProgram{}
	out := make([]CodeSummary, 0, len(codes))
	for _, c := range codes {
		p, err := s.programFor(s.repo, c.ProgramID, c.Kind, programs)
		if err != nil {
			return nil, totals, err
		}
		pv, err := s.programView(p)
		if err != nil {
			return nil, totals, err
		}
		n := counts[c.ID]
		e := p.Earnings(int(n.Qualified))
		ev := EarningsView{ReferralEarnings: e, PaidOut: n.PaidOut, Owed: domain.Zero(), Overpaid: domain.Zero()}
		if n.PaidOut.LessThan(e.Earned) {
			ev.Owed = e.Earned.Sub(n.PaidOut)
		} else {
			ev.Overpaid = n.PaidOut.Sub(e.Earned)
		}
		f := Funnel{
			Visitors: n.Visitors, Visits: n.Visits, Signups: n.Signups, Pending: n.Pending,
			Qualified: n.Qualified, Rejected: n.Rejected,
			VisitorToSignupRate:   pct(n.Signups, n.Visitors),
			SignupToQualifiedRate: pct(n.Qualified, n.Signups),
		}
		out = append(out, CodeSummary{
			Code:     CodeView{ID: c.ID, Code: c.Code, Kind: c.Kind, Label: c.Label, Status: c.Status, Link: s.link(c.Code), CreatedAt: c.CreatedAt},
			Program:  pv,
			Funnel:   f,
			Earnings: ev,
		})
		totals.Funnel.Visitors += f.Visitors
		totals.Funnel.Visits += f.Visits
		totals.Funnel.Signups += f.Signups
		totals.Funnel.Pending += f.Pending
		totals.Funnel.Qualified += f.Qualified
		totals.Funnel.Rejected += f.Rejected
		totals.Earned = totals.Earned.Add(e.Earned)
		totals.PaidOut = totals.PaidOut.Add(n.PaidOut)
		totals.Owed = totals.Owed.Add(ev.Owed)
	}
	totals.Funnel.VisitorToSignupRate = pct(totals.Funnel.Signups, totals.Funnel.Visitors)
	totals.Funnel.SignupToQualifiedRate = pct(totals.Funnel.Qualified, totals.Funnel.Signups)
	return out, totals, nil
}

// ownedCodes returns the caller's codes, optionally narrowed to one (which must be theirs).
func (s *Service) ownedCodes(userID uuid.UUID, codeID *uuid.UUID) ([]domain.ReferralCode, error) {
	codes, err := s.repo.ListCodesByOwner(userID)
	if err != nil {
		return nil, err
	}
	if codeID == nil {
		return codes, nil
	}
	for _, c := range codes {
		if c.ID == *codeID {
			return []domain.ReferralCode{c}, nil
		}
	}
	return nil, domain.NewAppError(404, "Referral code not found", domain.ErrNotFound)
}

func codeIDs(codes []domain.ReferralCode) []uuid.UUID {
	ids := make([]uuid.UUID, len(codes))
	for i := range codes {
		ids[i] = codes[i].ID
	}
	return ids
}

// MyDashboard is the agent's own summary: link, funnel, threshold progress and earnings.
func (s *Service) MyDashboard(ctx context.Context, userID uuid.UUID) (*AgentDashboard, error) {
	s.refresh(ctx)
	codes, err := s.repo.ListCodesByOwner(userID)
	if err != nil {
		return nil, err
	}
	sums, totals, err := s.summarize(codes)
	if err != nil {
		return nil, err
	}
	return &AgentDashboard{IsReferrer: len(codes) > 0, Codes: sums, Totals: totals}, nil
}

// ReferralView is one referred account as an agent sees it (masked) or staff see it (full).
type ReferralView struct {
	ID          uuid.UUID  `json:"id"`
	Code        string     `json:"code"`
	Name        string     `json:"name"`
	Phone       string     `json:"phone"`
	Status      string     `json:"status"`
	SignedUpAt  time.Time  `json:"signedUpAt"`
	QualifiedAt *time.Time `json:"qualifiedAt,omitempty"`
	// QualifiesBy is the end of the attribution window for a pending referral.
	QualifiesBy  *time.Time `json:"qualifiesBy,omitempty"`
	Expired      bool       `json:"expired"` // pending and past the window: can no longer qualify
	NeedsPhone   bool       `json:"needsPhone"`
	AuctionTitle string     `json:"auctionTitle,omitempty"`
	RejectReason string     `json:"rejectReason,omitempty"`
	RejectNote   string     `json:"rejectNote,omitempty"`
	// Staff only.
	UserID     *uuid.UUID `json:"userId,omitempty"`
	SameDevice int64      `json:"sameDevice,omitempty"`
}

type ReferralPage struct {
	Items []ReferralView `json:"items"`
	Total int64          `json:"total"`
	Page  int            `json:"page"`
	Limit int            `json:"limit"`
}

func (s *Service) referralPage(codes []domain.ReferralCode, f ports.ReferralAttributionFilter, staff bool) (*ReferralPage, error) {
	if f.Status != "" && f.Status != domain.ReferralSignedUp && f.Status != domain.ReferralQualified && f.Status != domain.ReferralRejected {
		return nil, badRequest("status must be signed_up, qualified or rejected")
	}
	rows, total, err := s.repo.ListAttributions(codeIDs(codes), f)
	if err != nil {
		return nil, err
	}
	windows := map[uuid.UUID]int{}
	programs := map[string]*domain.ReferralProgram{}
	for _, c := range codes {
		p, err := s.programFor(s.repo, c.ProgramID, c.Kind, programs)
		if err != nil {
			return nil, err
		}
		windows[c.ID] = p.AttributionWindowDays
	}
	now := s.now()
	page := &ReferralPage{Items: make([]ReferralView, 0, len(rows)), Total: total, Page: max(f.Page, 1), Limit: f.Limit}
	if page.Limit <= 0 || page.Limit > 100 {
		page.Limit = 20
	}
	for _, r := range rows {
		v := ReferralView{
			ID: r.ID, Code: r.Code, Status: r.Status, SignedUpAt: r.SignedUpAt, QualifiedAt: r.QualifiedAt,
			AuctionTitle: r.AuctionTitle, RejectReason: r.RejectReason,
		}
		if r.Status == domain.ReferralSignedUp {
			by := r.SignedUpAt.Add(time.Duration(windows[r.CodeID]) * 24 * time.Hour)
			v.QualifiesBy = &by
			v.Expired = now.After(by)
			v.NeedsPhone = domain.PhoneDigits(r.UserPhone) == ""
		}
		if staff {
			uid := r.ReferredUserID
			v.Name, v.Phone, v.UserID, v.SameDevice, v.RejectNote = r.UserName, r.UserPhone, &uid, r.SameDevice, r.RejectNote
		} else {
			v.Name, v.Phone = domain.MaskName(r.UserName), domain.MaskPhone(r.UserPhone)
		}
		page.Items = append(page.Items, v)
	}
	return page, nil
}

// MyReferrals lists the accounts the agent brought in, masked.
func (s *Service) MyReferrals(ctx context.Context, userID uuid.UUID, codeID *uuid.UUID, f ports.ReferralAttributionFilter) (*ReferralPage, error) {
	codes, err := s.ownedCodes(userID, codeID)
	if err != nil {
		return nil, err
	}
	return s.referralPage(codes, f, false)
}

const maxActivityDays = 90

func activitySince(now time.Time, days int) time.Time {
	if days <= 0 {
		days = 30
	}
	if days > maxActivityDays {
		days = maxActivityDays
	}
	return now.AddDate(0, 0, -(days - 1))
}

// MyActivity is the agent's day-by-day funnel for charts.
func (s *Service) MyActivity(ctx context.Context, userID uuid.UUID, codeID *uuid.UUID, days int) ([]ports.ReferralDailyCount, error) {
	codes, err := s.ownedCodes(userID, codeID)
	if err != nil {
		return nil, err
	}
	return s.repo.Activity(codeIDs(codes), activitySince(s.now(), days))
}

// MyPayouts lists what staff paid the agent.
func (s *Service) MyPayouts(ctx context.Context, userID uuid.UUID) ([]domain.ReferralPayout, error) {
	codes, err := s.ownedCodes(userID, nil)
	if err != nil {
		return nil, err
	}
	return s.repo.ListPayouts(codeIDs(codes))
}

// ---- Staff views ----

func (s *Service) code(id uuid.UUID) (*domain.ReferralCode, error) {
	c, err := s.repo.GetCode(id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.NewAppError(404, "Referral code not found", domain.ErrNotFound)
	}
	return c, err
}

// CodeDetail is one code's summary for staff, with its owner.
type CodeDetail struct {
	CodeSummary
	OwnerUserID *uuid.UUID `json:"ownerUserId,omitempty"`
	OwnerName   string     `json:"ownerName"`
	OwnerPhone  string     `json:"ownerPhone"`
}

func (s *Service) CodeDetail(ctx context.Context, id uuid.UUID) (*CodeDetail, error) {
	s.refresh(ctx)
	c, err := s.code(id)
	if err != nil {
		return nil, err
	}
	sums, _, err := s.summarize([]domain.ReferralCode{*c})
	if err != nil {
		return nil, err
	}
	d := &CodeDetail{CodeSummary: sums[0], OwnerUserID: c.OwnerUserID}
	if c.OwnerUserID != nil {
		if u, err := s.users.GetByID(*c.OwnerUserID); err == nil && u != nil {
			d.OwnerName, d.OwnerPhone = u.FullName, u.Phone
		}
	}
	return d, nil
}

func (s *Service) CodeReferrals(ctx context.Context, id uuid.UUID, f ports.ReferralAttributionFilter) (*ReferralPage, error) {
	c, err := s.code(id)
	if err != nil {
		return nil, err
	}
	return s.referralPage([]domain.ReferralCode{*c}, f, true)
}

func (s *Service) CodeActivity(ctx context.Context, id uuid.UUID, days int) ([]ports.ReferralDailyCount, error) {
	c, err := s.code(id)
	if err != nil {
		return nil, err
	}
	return s.repo.Activity([]uuid.UUID{c.ID}, activitySince(s.now(), days))
}

func (s *Service) CodePayouts(ctx context.Context, id uuid.UUID) ([]domain.ReferralPayout, error) {
	c, err := s.code(id)
	if err != nil {
		return nil, err
	}
	return s.repo.ListPayouts([]uuid.UUID{c.ID})
}

// CodeListItem is a row of the staff code list, with earnings.
type CodeListItem struct {
	ports.ReferralCodeRow
	Link      string       `json:"link"`
	Threshold int          `json:"threshold"`
	Unlocked  bool         `json:"unlocked"`
	Earned    domain.Money `json:"earned"`
	Owed      domain.Money `json:"owed"`
}

type CodeListPage struct {
	Items []CodeListItem `json:"items"`
	Total int64          `json:"total"`
	Page  int            `json:"page"`
	Limit int            `json:"limit"`
}

func (s *Service) ListCodes(ctx context.Context, f ports.ReferralCodeFilter) (*CodeListPage, error) {
	s.refresh(ctx)
	rows, total, err := s.repo.ListCodes(f)
	if err != nil {
		return nil, err
	}
	page := &CodeListPage{Items: make([]CodeListItem, 0, len(rows)), Total: total, Page: max(f.Page, 1), Limit: f.Limit}
	if page.Limit <= 0 || page.Limit > 100 {
		page.Limit = 20
	}
	programs := map[string]*domain.ReferralProgram{}
	for _, r := range rows {
		p, err := s.programFor(s.repo, r.ProgramID, r.Kind, programs)
		if err != nil {
			return nil, err
		}
		e := p.Earnings(int(r.Qualified))
		owed := domain.Zero()
		if r.PaidOut.LessThan(e.Earned) {
			owed = e.Earned.Sub(r.PaidOut)
		}
		page.Items = append(page.Items, CodeListItem{
			ReferralCodeRow: r, Link: s.link(r.Code), Threshold: p.Threshold, Unlocked: e.Unlocked, Earned: e.Earned, Owed: owed,
		})
	}
	return page, nil
}
