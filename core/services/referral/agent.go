package referral

import (
	"context"
	"errors"
	"strings"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

// Self-serve agents: a participant becomes an agent from the app and gets a code straight away.
// See domain/referral_agent.go for the rules.

// Reasons an account can't become an agent right now (the app shows them in plain words).
const (
	JoinBlockedClosed     = "signup_closed"   // staff paused self-serve agent signup
	JoinBlockedNotAllowed = "not_participant" // staff and other non-participant accounts
	JoinBlockedSuspended  = "account_inactive"
	JoinBlockedNeedsPhone = "needs_phone"  // add and verify a phone first
	JoinBlockedPhoneTaken = "phone_taken"  // another agent uses this phone
	JoinBlockedPaused     = "agent_paused" // already an agent, and staff paused the code
)

// AgentJoinInfo is GET /referrals/agent: whether the caller can become an agent and the deal
// they would get.
type AgentJoinInfo struct {
	Open     bool        `json:"open"`
	IsAgent  bool        `json:"isAgent"`
	CanJoin  bool        `json:"canJoin"`
	Reason   string      `json:"reason,omitempty"`
	HasPhone bool        `json:"hasPhone"`
	Phone    string      `json:"phone,omitempty"` // masked
	FullName string      `json:"fullName"`
	Program  ProgramView `json:"program"`
}

// JoinInput is POST /referrals/agent. VerifiedPhone is set by the handler from a phone
// verification token, for accounts that have no phone yet (Telegram or Google sign-ins).
type JoinInput struct {
	AcceptTerms   bool
	VerifiedPhone string
}

func (s *Service) agentCodeOf(repo ports.ReferralRepository, userID uuid.UUID) (*domain.ReferralCode, error) {
	codes, err := repo.ListCodesByOwner(userID)
	if err != nil {
		return nil, err
	}
	for i := range codes {
		if codes[i].Kind == domain.ReferralKindAgent {
			return &codes[i], nil
		}
	}
	return nil, nil
}

// joinBlock returns why u can't become an agent ("" when they can). phone is the number the
// agent will be known by: the account's, or a newly verified one.
func (s *Service) joinBlock(repo ports.ReferralRepository, u *domain.User, phone string, open bool) (string, error) {
	if u.Role != domain.RoleParticipant {
		return JoinBlockedNotAllowed, nil
	}
	if u.Status != domain.StatusActive {
		return JoinBlockedSuspended, nil
	}
	if !open {
		return JoinBlockedClosed, nil
	}
	digits := domain.PhoneDigits(phone)
	if len(digits) < 9 {
		return JoinBlockedNeedsPhone, nil
	}
	taken, err := repo.AgentPhoneTaken(digits, u.ID)
	if err != nil {
		return "", err
	}
	if taken {
		return JoinBlockedPhoneTaken, nil
	}
	return "", nil
}

// AgentProgramInfo is the public pitch for visitors who aren't signed in: whether agent signup
// is open and the deal a new agent gets.
type AgentProgramInfo struct {
	Open    bool        `json:"open"`
	Program ProgramView `json:"program"`
}

func (s *Service) AgentProgram(ctx context.Context) (*AgentProgramInfo, error) {
	settings, err := s.repo.GetSettings()
	if err != nil {
		return nil, err
	}
	program, err := s.repo.GetDefaultProgram(domain.ReferralKindAgent)
	if err != nil {
		return nil, err
	}
	pv, err := s.programView(program)
	if err != nil {
		return nil, err
	}
	return &AgentProgramInfo{Open: settings.AgentSignupOpen, Program: pv}, nil
}

// AgentJoinInfo tells the app what to show on the "Become an agent" screen.
func (s *Service) AgentJoinInfo(ctx context.Context, userID uuid.UUID) (*AgentJoinInfo, error) {
	u, err := s.users.GetByID(userID)
	if err != nil {
		return nil, err
	}
	settings, err := s.repo.GetSettings()
	if err != nil {
		return nil, err
	}
	program, err := s.repo.GetDefaultProgram(domain.ReferralKindAgent)
	if err != nil {
		return nil, err
	}
	pv, err := s.programView(program)
	if err != nil {
		return nil, err
	}
	info := &AgentJoinInfo{
		Open: settings.AgentSignupOpen, HasPhone: strings.TrimSpace(u.Phone) != "",
		FullName: u.FullName, Program: pv,
	}
	if info.HasPhone {
		info.Phone = domain.MaskPhone(u.Phone)
	}
	code, err := s.agentCodeOf(s.repo, userID)
	if err != nil {
		return nil, err
	}
	if code != nil {
		info.IsAgent = true
		if code.Status != domain.ReferralCodeActive {
			info.Reason = JoinBlockedPaused
		}
		return info, nil
	}
	if info.Reason, err = s.joinBlock(s.repo, u, u.Phone, settings.AgentSignupOpen); err != nil {
		return nil, err
	}
	info.CanJoin = info.Reason == ""
	return info, nil
}

var joinMessages = map[string]string{
	JoinBlockedClosed:     "New agent signups are paused right now. Please try again later.",
	JoinBlockedNotAllowed: "Only customer accounts can become agents.",
	JoinBlockedSuspended:  "Your account isn't active.",
	JoinBlockedNeedsPhone: "Add and verify your phone number first.",
	JoinBlockedPhoneTaken: "Another agent already uses this phone number.",
}

// JoinAsAgent makes the caller an agent: it creates their code on the default agent program and
// returns their dashboard. It is idempotent: an existing agent gets their dashboard back.
func (s *Service) JoinAsAgent(ctx context.Context, userID uuid.UUID, auditCtx *domain.AuditContext, in JoinInput) (*AgentDashboard, error) {
	if !in.AcceptTerms {
		return nil, badRequest("Please accept the agent rules")
	}
	var created *domain.ReferralCode
	err := s.repo.WithAgentLock(func(repo ports.ReferralRepository) error {
		existing, err := s.agentCodeOf(repo, userID)
		if err != nil {
			return err
		}
		if existing != nil {
			return nil
		}
		u, err := s.users.GetByID(userID)
		if err != nil {
			return err
		}
		phone := strings.TrimSpace(u.Phone)
		newPhone := ""
		if phone == "" && strings.TrimSpace(in.VerifiedPhone) != "" {
			newPhone = strings.TrimSpace(in.VerifiedPhone)
			if other, err := s.users.GetByPhone(newPhone); err == nil && other != nil && other.ID != u.ID {
				return domain.NewAppError(409, "That phone number belongs to another account", domain.ErrConflict)
			}
			phone = newPhone
		}
		settings, err := repo.GetSettings()
		if err != nil {
			return err
		}
		reason, err := s.joinBlock(repo, u, phone, settings.AgentSignupOpen)
		if err != nil {
			return err
		}
		if reason != "" {
			status := 400
			if reason == JoinBlockedClosed || reason == JoinBlockedNotAllowed || reason == JoinBlockedSuspended {
				status = 403
			}
			return domain.NewAppError(status, joinMessages[reason], domain.ErrInvalidInput)
		}
		if newPhone != "" {
			u.Phone = newPhone
			if err := s.users.Update(u); err != nil {
				return err
			}
		}
		c := &domain.ReferralCode{
			Kind: domain.ReferralKindAgent, OwnerUserID: &u.ID, Label: clip(u.FullName, 120),
			Status: domain.ReferralCodeActive, CreatedBy: &u.ID,
		}
		if c.Label == "" {
			c.Label = "Agent"
		}
		for attempt := 0; attempt < 5; attempt++ {
			if c.Code, err = domain.GenerateReferralCode(); err != nil {
				return err
			}
			c.ID = uuid.Nil
			if err = repo.CreateCode(c); !errors.Is(err, domain.ErrConflict) {
				break
			}
		}
		if err != nil {
			return err
		}
		created = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created != nil {
		s.auditLog(userID, auditCtx, "AGENT_SELF_SIGNUP", "referral_code", created.ID.String(), map[string]interface{}{"code": created.Code})
		s.log.Info("referral.agent_joined", "code", created.Code, "user_id", userID.String())
	}
	return s.MyDashboard(ctx, userID)
}

// SetMyPayoutAccount saves where the caller wants their earnings sent. Each payout account can
// belong to one code only, so one person can't run several agent accounts into one wallet.
func (s *Service) SetMyPayoutAccount(ctx context.Context, userID uuid.UUID, codeID *uuid.UUID, auditCtx *domain.AuditContext, in domain.ReferralPayoutAccount) (*domain.ReferralPayoutAccount, error) {
	p := in.Normalize()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	codes, err := s.ownedCodes(userID, codeID)
	if err != nil {
		return nil, err
	}
	if len(codes) == 0 {
		return nil, domain.NewAppError(404, "You are not an agent yet", domain.ErrNotFound)
	}
	c := codes[0]
	err = s.repo.WithAgentLock(func(repo ports.ReferralRepository) error {
		taken, err := repo.PayoutAccountTaken(p, c.ID)
		if err != nil {
			return err
		}
		if taken {
			return domain.NewAppError(409, "That payout account is already used by another agent", domain.ErrConflict)
		}
		return repo.SetPayoutAccount(c.ID, p)
	})
	if err != nil {
		return nil, err
	}
	s.auditLog(userID, auditCtx, "AGENT_PAYOUT_ACCOUNT", "referral_code", c.ID.String(), map[string]interface{}{
		"before": payoutOf(&c), "after": p,
	})
	return &p, nil
}

// payoutOf returns the code's payout account, or nil when none is set.
func payoutOf(c *domain.ReferralCode) *domain.ReferralPayoutAccount {
	if c.PayoutAccount == "" {
		return nil
	}
	return &domain.ReferralPayoutAccount{Method: c.PayoutMethod, Bank: c.PayoutBank, Account: c.PayoutAccount, Name: c.PayoutName}
}

// ---- Staff ----

func (s *Service) Settings(ctx context.Context) (*domain.ReferralSettings, error) {
	return s.repo.GetSettings()
}

// SettingsInput is PUT /referral-settings; omitted fields keep their value.
type SettingsInput struct {
	AgentSignupOpen *bool `json:"agentSignupOpen"`
}

func (s *Service) UpdateSettings(ctx context.Context, actor uuid.UUID, auditCtx *domain.AuditContext, in SettingsInput) (*domain.ReferralSettings, error) {
	st, err := s.repo.GetSettings()
	if err != nil {
		return nil, err
	}
	before := *st
	if in.AgentSignupOpen != nil {
		st.AgentSignupOpen = *in.AgentSignupOpen
	}
	st.UpdatedBy = &actor
	if err := s.repo.UpdateSettings(st); err != nil {
		return nil, err
	}
	s.auditLog(actor, auditCtx, "UPDATE_REFERRAL_SETTINGS", "referral_settings", "1", map[string]interface{}{
		"before": map[string]interface{}{"agentSignupOpen": before.AgentSignupOpen},
		"after":  map[string]interface{}{"agentSignupOpen": st.AgentSignupOpen},
	})
	return st, nil
}
