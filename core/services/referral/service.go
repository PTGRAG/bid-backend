// Package referral runs agent and influencer referral codes: link clicks, attributing new
// signups, qualifying them once they buy a paid ticket (with the anti-fraud rules), payout
// earnings by tier, and the data behind the agent dashboard and the staff screens.
// See domain/referral.go for the rules.
package referral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

type Service struct {
	repo   ports.ReferralRepository
	users  ports.UserRepository
	payer  ports.PayerAccountResolver
	audit  ports.AuditLogService
	appURL string
	now    func() time.Time
	log    *slog.Logger
}

func NewService(repo ports.ReferralRepository, users ports.UserRepository, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, users: users, now: time.Now, log: log}
}

// SetPayerAccountResolver enables the one-referral-per-payment-account rule.
func (s *Service) SetPayerAccountResolver(p ports.PayerAccountResolver) { s.payer = p }

// SetAuditLog records staff actions (codes, programs, payouts, rejections) in the audit log.
func (s *Service) SetAuditLog(a ports.AuditLogService) { s.audit = a }

// SetAppURL is the participant site origin used to build share links.
func (s *Service) SetAppURL(u string) { s.appURL = strings.TrimRight(strings.TrimSpace(u), "/") }

// SetClock overrides the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func badRequest(msg string) error { return domain.NewAppError(400, msg, domain.ErrInvalidInput) }

var errCodeNotFound = domain.NewAppError(404, "This invite code is not valid", domain.ErrNotFound)

// ---- Public: link clicks ----

var visitorIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// PublicCode is what a visitor sees about a code ("Invited by Abebe K.").
type PublicCode struct {
	Code      string `json:"code"`
	Kind      string `json:"kind"`
	InvitedBy string `json:"invitedBy"`
}

func (s *Service) activeCode(raw string) (*domain.ReferralCode, error) {
	code := domain.NormalizeReferralCode(raw)
	if code == "" || len(code) > 20 {
		return nil, errCodeNotFound
	}
	c, err := s.repo.GetCodeByCode(code)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && c.Status != domain.ReferralCodeActive) {
		return nil, errCodeNotFound
	}
	return c, err
}

func (s *Service) publicView(c *domain.ReferralCode) *PublicCode {
	invitedBy := strings.TrimSpace(c.Label)
	if c.OwnerUserID != nil && s.users != nil {
		if u, err := s.users.GetByID(*c.OwnerUserID); err == nil && u != nil {
			if c.Kind == domain.ReferralKindAgent || invitedBy == "" {
				invitedBy = domain.MaskName(u.FullName)
			}
		}
	}
	return &PublicCode{Code: c.Code, Kind: c.Kind, InvitedBy: invitedBy}
}

// LookupCode validates a code from a link.
func (s *Service) LookupCode(ctx context.Context, raw string) (*PublicCode, error) {
	c, err := s.activeCode(raw)
	if err != nil {
		return nil, err
	}
	return s.publicView(c), nil
}

// RecordClick counts a visit through a code's link. A visitor (one browser) is counted once per
// code however many times they open it; the hit counter keeps the total.
func (s *Service) RecordClick(ctx context.Context, raw, visitorID, ip, userAgent, landingPath string) (*PublicCode, error) {
	c, err := s.activeCode(raw)
	if err != nil {
		return nil, err
	}
	visitorID = strings.TrimSpace(visitorID)
	if !visitorIDPattern.MatchString(visitorID) {
		return nil, badRequest("visitorId must be 8-64 letters, digits, '-' or '_'")
	}
	if err := s.repo.UpsertClick(&domain.ReferralClick{
		CodeID:      c.ID,
		VisitorID:   visitorID,
		IPHash:      hashIP(ip),
		UserAgent:   clip(userAgent, 255),
		LandingPath: clip(landingPath, 255),
	}); err != nil {
		return nil, err
	}
	return s.publicView(c), nil
}

func hashIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("carra-referral:" + ip))
	return hex.EncodeToString(sum[:16])
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- Participant: attach a code after signing up ----

// ClaimResult tells the client whether the account was attributed to the code. Reasons when
// not: already_referred, existing_account (created too long ago), existing_customer (already
// bought a ticket), not_participant.
type ClaimResult struct {
	Attributed bool   `json:"attributed"`
	Code       string `json:"code"`
	Reason     string `json:"reason,omitempty"`
}

// Claim attributes a newly created account to a code. Clients call it right after signup (or
// the first sign-in through Google or Telegram) when the visitor arrived through a link. It is
// idempotent; an account is attributed to the first code it claims.
func (s *Service) Claim(ctx context.Context, userID uuid.UUID, raw, visitorID string) (*ClaimResult, error) {
	c, err := s.activeCode(raw)
	if err != nil {
		return nil, err
	}
	res := &ClaimResult{Code: c.Code}
	if existing, err := s.repo.GetAttributionByUser(userID); err == nil {
		res.Attributed = existing.CodeID == c.ID
		if !res.Attributed {
			res.Reason = "already_referred"
		}
		return res, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if c.OwnerUserID != nil && *c.OwnerUserID == userID {
		return nil, badRequest("You can't use your own invite code")
	}
	u, err := s.users.GetByID(userID)
	if err != nil {
		return nil, err
	}
	if u.Role != domain.RoleParticipant {
		res.Reason = "not_participant"
		return res, nil
	}
	if s.now().Sub(u.CreatedAt) > domain.ReferralClaimWindow {
		res.Reason = "existing_account"
		return res, nil
	}
	if paid, err := s.repo.HasPaidEntry(userID); err != nil {
		return nil, err
	} else if paid {
		res.Reason = "existing_customer"
		return res, nil
	}
	if !visitorIDPattern.MatchString(strings.TrimSpace(visitorID)) {
		visitorID = ""
	}
	err = s.repo.CreateAttribution(&domain.ReferralAttribution{
		CodeID:         c.ID,
		ReferredUserID: userID,
		VisitorID:      strings.TrimSpace(visitorID),
		Status:         domain.ReferralSignedUp,
		SignedUpAt:     u.CreatedAt,
	})
	if errors.Is(err, domain.ErrConflict) {
		// A concurrent claim won; report what is stored.
		if existing, gErr := s.repo.GetAttributionByUser(userID); gErr == nil {
			res.Attributed = existing.CodeID == c.ID
			if !res.Attributed {
				res.Reason = "already_referred"
			}
			return res, nil
		}
	}
	if err != nil {
		return nil, err
	}
	res.Attributed = true
	s.log.Info("referral.claimed", "code", c.Code, "user_id", userID.String())
	return res, nil
}

// ---- Qualification sweep ----

// SweepResult counts what one sweep changed.
type SweepResult struct {
	Qualified int  `json:"qualified"`
	Rejected  int  `json:"rejected"`
	Lapsed    int  `json:"lapsed"`  // qualified referrals whose ticket was refunded, back to pending
	Skipped   bool `json:"skipped"` // another sweep was running
}

const sweepBatch = 500

// Sweep qualifies pending referrals that bought a paid ticket and applies the anti-fraud rules.
// It runs every minute from the background job and before dashboard reads; concurrent calls
// are serialized by a database lock (a call that finds the lock taken returns Skipped).
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	ran, err := s.repo.WithSweepLock(func(tx ports.ReferralRepository) error {
		lapsed, err := tx.LapsedQualified(sweepBatch)
		if err != nil {
			return err
		}
		for _, id := range lapsed {
			if err := tx.MarkSignedUp(id); err != nil {
				return err
			}
			res.Lapsed++
		}
		cands, err := tx.PendingCandidates(sweepBatch)
		if err != nil {
			return err
		}
		programs := map[string]*domain.ReferralProgram{}
		for i := range cands {
			if err := ctx.Err(); err != nil {
				return err
			}
			outcome, err := s.evaluate(tx, &cands[i], programs)
			if err != nil {
				s.log.Warn("referral.sweep.evaluate_failed", "attribution_id", cands[i].AttributionID.String(), "error", err)
				continue
			}
			switch outcome {
			case domain.ReferralQualified:
				res.Qualified++
			case domain.ReferralRejected:
				res.Rejected++
			}
		}
		return nil
	})
	res.Skipped = !ran && err == nil
	if err == nil && (res.Qualified > 0 || res.Rejected > 0 || res.Lapsed > 0) {
		s.log.Info("referral.sweep", "qualified", res.Qualified, "rejected", res.Rejected, "lapsed", res.Lapsed)
	}
	return res, err
}

func (s *Service) programFor(repo ports.ReferralRepository, programID *uuid.UUID, kind string, cache map[string]*domain.ReferralProgram) (*domain.ReferralProgram, error) {
	key := "default:" + kind
	if programID != nil {
		key = programID.String()
	}
	if p, ok := cache[key]; ok {
		return p, nil
	}
	var p *domain.ReferralProgram
	var err error
	if programID != nil {
		p, err = repo.GetProgram(*programID)
	}
	if programID == nil || errors.Is(err, domain.ErrNotFound) {
		p, err = repo.GetDefaultProgram(kind)
	}
	if err != nil {
		return nil, err
	}
	if cache != nil {
		cache[key] = p
	}
	return p, nil
}

// evaluate decides one pending referral: still pending (""), qualified or rejected.
func (s *Service) evaluate(tx ports.ReferralRepository, c *ports.ReferralCandidate, programs map[string]*domain.ReferralProgram) (string, error) {
	program, err := s.programFor(tx, c.ProgramID, c.Kind, programs)
	if err != nil {
		return "", err
	}
	deadline := c.SignedUpAt.Add(time.Duration(program.AttributionWindowDays) * 24 * time.Hour)
	pay, err := tx.FirstQualifyingPayment(c.ReferredUserID, c.SignedUpAt, deadline, program.QualifyingAuctionIDs)
	if err != nil || pay == nil {
		return "", err
	}
	phone := domain.PhoneDigits(c.UserPhone)
	if phone == "" {
		// Stays pending until the account has a phone number to check.
		return "", nil
	}
	if (c.OwnerUserID != nil && *c.OwnerUserID == c.ReferredUserID) || (c.OwnerPhone != "" && domain.PhoneDigits(c.OwnerPhone) == phone) {
		return domain.ReferralRejected, tx.MarkRejected(c.AttributionID, domain.ReferralRejectSelf, "")
	}
	if taken, err := tx.PhoneQualified(phone); err != nil {
		return "", err
	} else if taken {
		return domain.ReferralRejected, tx.MarkRejected(c.AttributionID, domain.ReferralRejectDuplicatePhone, "")
	}
	account := ""
	if s.payer != nil {
		account, err = s.payer.PayerAccount(pay.TxRef)
		if err != nil {
			// Try again on the next sweep rather than qualify without the account check.
			return "", err
		}
		if taken, err := tx.AccountQualified(account); err != nil {
			return "", err
		} else if taken {
			return domain.ReferralRejected, tx.MarkRejected(c.AttributionID, domain.ReferralRejectDuplicateAccount, "")
		}
	}
	err = tx.MarkQualified(c.AttributionID, *pay, phone, account, s.now().UTC())
	if errors.Is(err, domain.ErrConflict) {
		reason := domain.ReferralRejectDuplicatePhone
		if taken, _ := tx.PhoneQualified(phone); !taken {
			reason = domain.ReferralRejectDuplicateAccount
		}
		return domain.ReferralRejected, tx.MarkRejected(c.AttributionID, reason, "")
	}
	if err != nil {
		return "", err
	}
	return domain.ReferralQualified, nil
}

// refresh runs a sweep before a read so dashboards show fresh numbers; failures only log.
func (s *Service) refresh(ctx context.Context) {
	if _, err := s.Sweep(ctx); err != nil {
		s.log.Warn("referral.refresh_failed", "error", err)
	}
}
