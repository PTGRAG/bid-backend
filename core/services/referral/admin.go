package referral

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// Staff actions. Callers check the staff role; every change is written to the audit log.

func (s *Service) auditLog(actor uuid.UUID, auditCtx *domain.AuditContext, action, entity, entityID string, meta map[string]interface{}) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Log(&domain.AuditLog{UserID: actor, Action: action, Entity: entity, EntityID: entityID, Metadata: meta}, auditCtx)
}

// ProgramAdminView adds the guardrail context staff need when editing payouts.
type ProgramAdminView struct {
	ProgramView
	NetMarginPerBid domain.Money `json:"netMarginPerBid"`
	TopTierAmount   domain.Money `json:"topTierAmount"`
	// LowestEntranceFee is the cheapest ticket among the qualifying auctions (or open auctions
	// when none are listed). The margin per bid can never be more than the ticket price.
	LowestEntranceFee *domain.Money `json:"lowestEntranceFee,omitempty"`
	Warnings          []string      `json:"warnings"`
	CodeCount         int64         `json:"-"`
}

func (s *Service) programAdminView(p *domain.ReferralProgram) (*ProgramAdminView, error) {
	pv, err := s.programView(p)
	if err != nil {
		return nil, err
	}
	v := &ProgramAdminView{ProgramView: pv, NetMarginPerBid: p.NetMarginPerBid, TopTierAmount: p.TopTierAmount(), Warnings: []string{}}
	fee, err := s.repo.LowestEntranceFee(p.QualifyingAuctionIDs)
	if err != nil {
		return nil, err
	}
	if fee != nil && fee.IsPositive() {
		v.LowestEntranceFee = fee
		if fee.LessThan(p.NetMarginPerBid) {
			v.Warnings = append(v.Warnings, fmt.Sprintf(
				"The net margin per bid (%s ETB) is higher than the cheapest ticket (%s ETB). Lower it to what Carra really nets per ticket.",
				p.NetMarginPerBid.String(), fee.String()))
		}
	}
	for _, id := range p.QualifyingAuctionIDs {
		found := false
		for _, a := range pv.QualifyingAuctions {
			if a.ID == id && a.Title != "" {
				found = true
			}
		}
		if !found {
			v.Warnings = append(v.Warnings, "A qualifying auction no longer exists: "+id.String())
		}
	}
	return v, nil
}

func (s *Service) ListPrograms(ctx context.Context) ([]ProgramAdminView, error) {
	ps, err := s.repo.ListPrograms()
	if err != nil {
		return nil, err
	}
	out := make([]ProgramAdminView, 0, len(ps))
	for i := range ps {
		v, err := s.programAdminView(&ps[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// ProgramInput is a program as staff submit it. Omitted fields keep their current value on
// update (or the defaults on create).
type ProgramInput struct {
	Name                  *string               `json:"name"`
	Kind                  string                `json:"kind"`
	Threshold             *int                  `json:"threshold"`
	Tiers                 []domain.ReferralTier `json:"tiers"`
	NetMarginPerBid       *domain.Money         `json:"netMarginPerBid"`
	QualifyingAuctionIDs  *[]uuid.UUID          `json:"qualifyingAuctionIds"`
	AttributionWindowDays *int                  `json:"attributionWindowDays"`
}

func (in ProgramInput) apply(p *domain.ReferralProgram) {
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Threshold != nil {
		p.Threshold = *in.Threshold
	}
	if in.Tiers != nil {
		tiers := append([]domain.ReferralTier(nil), in.Tiers...)
		domain.SortTiers(tiers)
		p.Tiers = tiers
	}
	if in.NetMarginPerBid != nil {
		p.NetMarginPerBid = *in.NetMarginPerBid
	}
	if in.QualifyingAuctionIDs != nil {
		seen := map[uuid.UUID]bool{}
		ids := []uuid.UUID{}
		for _, id := range *in.QualifyingAuctionIDs {
			if id != uuid.Nil && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		p.QualifyingAuctionIDs = ids
	}
	if in.AttributionWindowDays != nil {
		p.AttributionWindowDays = *in.AttributionWindowDays
	}
}

func (s *Service) checkAuctions(ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	titles, err := s.repo.AuctionTitles(ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := titles[id]; !ok {
			return badRequest("Unknown qualifying auction: " + id.String())
		}
	}
	return nil
}

// CreateProgram adds a non-default program (e.g. a special deal for one influencer).
func (s *Service) CreateProgram(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, in ProgramInput) (*ProgramAdminView, error) {
	if !domain.ValidReferralKind(in.Kind) {
		return nil, badRequest("kind must be agent or influencer")
	}
	p := domain.NewDefaultReferralProgram(in.Kind)
	p.IsDefault = false
	in.apply(&p)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := s.checkAuctions(p.QualifyingAuctionIDs); err != nil {
		return nil, err
	}
	if err := s.repo.CreateProgram(&p); err != nil {
		return nil, err
	}
	s.auditLog(actor, auditCtx, "CREATE_REFERRAL_PROGRAM", "referral_program", p.ID.String(), map[string]interface{}{"name": p.Name, "kind": p.Kind})
	return s.programAdminView(&p)
}

// UpdateProgram changes payout rules. Earnings are computed from the current rules, so a change
// applies to every code on the program, including bidders already counted.
func (s *Service) UpdateProgram(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, id uuid.UUID, in ProgramInput) (*ProgramAdminView, error) {
	p, err := s.repo.GetProgram(id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.NewAppError(404, "Referral program not found", domain.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	before := *p
	in.Kind = p.Kind // kind is fixed
	in.apply(p)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := s.checkAuctions(p.QualifyingAuctionIDs); err != nil {
		return nil, err
	}
	if err := s.repo.UpdateProgram(p); err != nil {
		return nil, err
	}
	s.auditLog(actor, auditCtx, "UPDATE_REFERRAL_PROGRAM", "referral_program", p.ID.String(), map[string]interface{}{
		"before": map[string]interface{}{"threshold": before.Threshold, "tiers": before.Tiers, "netMarginPerBid": before.NetMarginPerBid.String(), "auctions": before.QualifyingAuctionIDs, "windowDays": before.AttributionWindowDays},
		"after":  map[string]interface{}{"threshold": p.Threshold, "tiers": p.Tiers, "netMarginPerBid": p.NetMarginPerBid.String(), "auctions": p.QualifyingAuctionIDs, "windowDays": p.AttributionWindowDays},
	})
	return s.programAdminView(p)
}

// CodeInput creates or edits a code. On create, the owner is found by phone (or user id); the
// code is generated unless a custom one is given (influencers often want their name).
type CodeInput struct {
	Kind        string     `json:"kind"`
	OwnerPhone  string     `json:"ownerPhone"`
	OwnerUserID *uuid.UUID `json:"ownerUserId"`
	Code        string     `json:"code"`
	Label       *string    `json:"label"`
	ProgramID   *uuid.UUID `json:"programId"`
	// ClearProgram sets the code back to its kind's default program.
	ClearProgram bool    `json:"clearProgram"`
	Status       *string `json:"status"`
}

func (s *Service) resolveOwner(in CodeInput) (*domain.User, error) {
	if in.OwnerUserID != nil {
		u, err := s.users.GetByID(*in.OwnerUserID)
		if err != nil || u == nil {
			return nil, badRequest("No account found for that owner")
		}
		return u, nil
	}
	phone := strings.TrimSpace(in.OwnerPhone)
	if phone == "" {
		return nil, nil
	}
	digits := domain.PhoneDigits(phone)
	for _, candidate := range []string{"+" + digits, phone} {
		if u, err := s.users.GetByPhone(candidate); err == nil && u != nil {
			return u, nil
		}
	}
	return nil, badRequest("No account uses that phone number. Ask the agent to sign up first.")
}

func (s *Service) checkProgram(id *uuid.UUID, kind string) error {
	if id == nil {
		return nil
	}
	p, err := s.repo.GetProgram(*id)
	if errors.Is(err, domain.ErrNotFound) {
		return badRequest("Unknown program")
	}
	if err != nil {
		return err
	}
	if p.Kind != kind {
		return badRequest("That program is for " + p.Kind + " codes")
	}
	return nil
}

func (s *Service) CreateCode(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, in CodeInput) (*CodeDetail, error) {
	if in.Kind == "" {
		in.Kind = domain.ReferralKindAgent
	}
	if !domain.ValidReferralKind(in.Kind) {
		return nil, badRequest("kind must be agent or influencer")
	}
	owner, err := s.resolveOwner(in)
	if err != nil {
		return nil, err
	}
	if owner == nil && in.Kind == domain.ReferralKindAgent {
		return nil, badRequest("An agent code needs the agent's account (their phone number)")
	}
	label := ""
	if in.Label != nil {
		label = clip(*in.Label, 120)
	}
	if label == "" && owner != nil {
		label = owner.FullName
	}
	if label == "" {
		return nil, badRequest("Give the code a label (e.g. the influencer's name)")
	}
	if err := s.checkProgram(in.ProgramID, in.Kind); err != nil {
		return nil, err
	}
	c := &domain.ReferralCode{Kind: in.Kind, Label: label, ProgramID: in.ProgramID, Status: domain.ReferralCodeActive, CreatedBy: &actor}
	if owner != nil {
		c.OwnerUserID = &owner.ID
	}
	if custom := domain.NormalizeReferralCode(in.Code); custom != "" {
		if err := domain.ValidateReferralCode(custom); err != nil {
			return nil, err
		}
		c.Code = custom
		err = s.repo.CreateCode(c)
	} else {
		for attempt := 0; attempt < 5; attempt++ {
			if c.Code, err = domain.GenerateReferralCode(); err != nil {
				return nil, err
			}
			c.ID = uuid.Nil
			if err = s.repo.CreateCode(c); !errors.Is(err, domain.ErrConflict) {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	s.auditLog(actor, auditCtx, "CREATE_REFERRAL_CODE", "referral_code", c.ID.String(), map[string]interface{}{"code": c.Code, "kind": c.Kind, "owner": c.OwnerUserID})
	return s.CodeDetail(ctx, c.ID)
}

func (s *Service) UpdateCode(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, id uuid.UUID, in CodeInput) (*CodeDetail, error) {
	c, err := s.code(id)
	if err != nil {
		return nil, err
	}
	if in.Label != nil {
		if l := clip(*in.Label, 120); l != "" {
			c.Label = l
		}
	}
	if in.Status != nil {
		if *in.Status != domain.ReferralCodeActive && *in.Status != domain.ReferralCodePaused {
			return nil, badRequest("status must be active or paused")
		}
		c.Status = *in.Status
	}
	if in.ClearProgram {
		c.ProgramID = nil
	} else if in.ProgramID != nil {
		if err := s.checkProgram(in.ProgramID, c.Kind); err != nil {
			return nil, err
		}
		c.ProgramID = in.ProgramID
	}
	if err := s.repo.UpdateCode(c); err != nil {
		return nil, err
	}
	s.auditLog(actor, auditCtx, "UPDATE_REFERRAL_CODE", "referral_code", c.ID.String(), map[string]interface{}{"status": c.Status, "label": c.Label, "program": c.ProgramID})
	return s.CodeDetail(ctx, c.ID)
}

// PayoutInput records money paid to a code owner outside the platform.
type PayoutInput struct {
	Amount    domain.Money `json:"amount"`
	Method    string       `json:"method"`
	Reference string       `json:"reference"`
	Note      string       `json:"note"`
}

// AddPayout records a payout. It may not exceed what is owed, so staff cannot overpay by mistake.
func (s *Service) AddPayout(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, codeID uuid.UUID, in PayoutInput) (*domain.ReferralPayout, error) {
	if !in.Amount.IsPositive() {
		return nil, badRequest("Amount must be more than 0")
	}
	detail, err := s.CodeDetail(ctx, codeID)
	if err != nil {
		return nil, err
	}
	if detail.Earnings.Owed.LessThan(in.Amount) {
		return nil, badRequest(fmt.Sprintf("That is more than is owed (%s ETB)", detail.Earnings.Owed.String()))
	}
	p := &domain.ReferralPayout{
		CodeID: codeID, Amount: in.Amount, Method: clip(in.Method, 40), Reference: clip(in.Reference, 120),
		Note: clip(in.Note, 255), PaidBy: &actor, PaidAt: s.now().UTC(),
	}
	if err := s.repo.CreatePayout(p); err != nil {
		return nil, err
	}
	s.auditLog(actor, auditCtx, "REFERRAL_PAYOUT", "referral_code", codeID.String(), map[string]interface{}{"amount": in.Amount.String(), "method": p.Method, "reference": p.Reference})
	return p, nil
}

// RejectReferral stops a referral from counting (staff found it fake).
func (s *Service) RejectReferral(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, id uuid.UUID, note string) error {
	if _, err := s.attribution(id); err != nil {
		return err
	}
	if err := s.repo.MarkRejected(id, domain.ReferralRejectManual, note); err != nil {
		return err
	}
	s.auditLog(actor, auditCtx, "REJECT_REFERRAL", "referral_attribution", id.String(), map[string]interface{}{"note": note})
	return nil
}

// RestoreReferral puts a rejected referral back to pending; the next sweep re-applies every rule,
// so a referral rejected as a duplicate stays rejected while the other one counts.
func (s *Service) RestoreReferral(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, id uuid.UUID) error {
	a, err := s.attribution(id)
	if err != nil {
		return err
	}
	if a.Status != domain.ReferralRejected {
		return badRequest("Only a rejected referral can be restored")
	}
	if err := s.repo.MarkSignedUp(id); err != nil {
		return err
	}
	s.auditLog(actor, auditCtx, "RESTORE_REFERRAL", "referral_attribution", id.String(), nil)
	s.refresh(ctx)
	return nil
}

func (s *Service) attribution(id uuid.UUID) (*domain.ReferralAttribution, error) {
	a, err := s.repo.GetAttribution(id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.NewAppError(404, "Referral not found", domain.ErrNotFound)
	}
	return a, err
}
