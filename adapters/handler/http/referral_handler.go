package http

import (
	"strconv"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/core/services/referral"
	"github.com/bid-hub/admin-backend/platform/apiresponse"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/google/uuid"
)

// ReferralHandler serves agent/influencer referral links, the agent dashboard data and the
// staff referral screens.
type ReferralHandler struct {
	svc *referral.Service
	// verifyPhone checks a phone verification token from /auth/verify-otp and returns its
	// phone; it lets an account without a phone add one while becoming an agent.
	verifyPhone func(token, purpose string) (string, error)
}

// SetPhoneVerifier wires the phone verification token check (AuthService.VerifyPhoneToken).
func (h *ReferralHandler) SetPhoneVerifier(fn func(token, purpose string) (string, error)) {
	h.verifyPhone = fn
}

func NewReferralHandler(svc *referral.Service) *ReferralHandler {
	return &ReferralHandler{svc: svc}
}

// registerPublicReferralRoutes must run before the authenticated group is created: that group
// sits on "/api" and would otherwise demand a token for these routes too.
func registerPublicReferralRoutes(api fiber.Router, h *ReferralHandler, limiterStorage fiber.Storage) {
	// A visitor opening an invite link. Visits never affect payouts (only qualified bidders do),
	// so the limit is loose: browser traffic arrives through the frontend's /api proxy and can
	// share one IP.
	clickLimit := limiter.New(limiter.Config{
		Max:        300,
		Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			return "limit:referral-click:" + c.IP() + ":" + strings.ToUpper(c.Params("code"))
		},
		LimitReached: func(c fiber.Ctx) error {
			return apiresponse.Fail(c, fiber.StatusTooManyRequests, "Too many requests. Please try again later.", "TOO_MANY_REQUESTS", nil)
		},
		Storage: limiterStorage,
	})
	api.Get("/public/referrals/:code", h.Lookup)
	api.Post("/public/referrals/:code/click", clickLimit, h.Click)
}

func registerReferralRoutes(protected fiber.Router, h *ReferralHandler) {
	// Signed in: attach a code to a new account, and the agent's own dashboard data.
	protected.Post("/referrals/claim", h.Claim)
	protected.Get("/referrals/me", h.MyDashboard)
	protected.Get("/referrals/me/referrals", h.MyReferrals)
	protected.Get("/referrals/me/activity", h.MyActivity)
	protected.Get("/referrals/me/payouts", h.MyPayouts)
	protected.Put("/referrals/me/payout-account", h.SetMyPayoutAccount)
	// Self-serve: become an agent.
	protected.Get("/referrals/agent", h.AgentJoinInfo)
	protected.Post("/referrals/agent", h.JoinAsAgent)

	// Staff.
	protected.Post("/referrals/sweep", h.Sweep)
	protected.Get("/referral-settings", h.GetSettings)
	protected.Put("/referral-settings", h.UpdateSettings)
	protected.Get("/referral-programs", h.ListPrograms)
	protected.Post("/referral-programs", h.CreateProgram)
	protected.Patch("/referral-programs/:id", h.UpdateProgram)
	protected.Get("/referral-codes", h.ListCodes)
	protected.Post("/referral-codes", h.CreateCode)
	protected.Get("/referral-codes/:id", h.GetCode)
	protected.Patch("/referral-codes/:id", h.UpdateCode)
	protected.Get("/referral-codes/:id/referrals", h.CodeReferrals)
	protected.Get("/referral-codes/:id/activity", h.CodeActivity)
	protected.Get("/referral-codes/:id/payouts", h.CodePayouts)
	protected.Post("/referral-codes/:id/payouts", h.AddPayout)
	protected.Post("/referral-attributions/:id/reject", h.Reject)
	protected.Post("/referral-attributions/:id/restore", h.Restore)
}

// requireStaffReader lets admins and compliance read referral data; changes need an admin.
func requireStaffReader(c fiber.Ctx) bool {
	switch GetUserRole(c) {
	case domain.RoleAdmin, domain.RoleSuperAdmin, domain.RoleCompliance:
		return true
	}
	_ = response.Error(c, fiber.StatusForbidden, "Staff access required", nil)
	return false
}

func currentUser(c fiber.Ctx) (uuid.UUID, bool) {
	id, err := uuid.Parse(GetUserID(c))
	if err != nil || id == uuid.Nil {
		_ = response.Error(c, fiber.StatusUnauthorized, "Authentication required", nil)
		return uuid.Nil, false
	}
	return id, true
}

func optionalCodeID(c fiber.Ctx) (*uuid.UUID, bool) {
	raw := strings.TrimSpace(c.Query("codeId"))
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		_ = validate.Fail(c, "Invalid codeId", validate.FieldError{Field: "codeId", Rule: "uuid", Message: "codeId must be a UUID"})
		return nil, false
	}
	return &id, true
}

func queryDays(c fiber.Ctx) int {
	n, _ := strconv.Atoi(c.Query("days"))
	return n
}

func attributionFilter(c fiber.Ctx) (ports.ReferralAttributionFilter, error) {
	page, limit, _, err := validate.BindPagination(c, 1, 20, 100)
	return ports.ReferralAttributionFilter{Status: strings.TrimSpace(c.Query("status")), Page: page, Limit: limit}, err
}

func referralError(c fiber.Ctx, err error) error {
	return mapLUBError(c, err)
}

// ---- Public ----

func (h *ReferralHandler) Lookup(c fiber.Ctx) error {
	pc, err := h.svc.LookupCode(c.Context(), c.Params("code"))
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral code", pc)
}

type referralClickRequest struct {
	VisitorID   string `json:"visitorId" validate:"required,min=8,max=64"`
	LandingPath string `json:"landingPath" validate:"omitempty,max=255"`
}

func (h *ReferralHandler) Click(c fiber.Ctx) error {
	var req referralClickRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}
	ac := GetAuditContext(c)
	pc, err := h.svc.RecordClick(c.Context(), c.Params("code"), req.VisitorID, ac.IPAddress, ac.UserAgent, req.LandingPath)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Visit recorded", pc)
}

// ---- Signed in ----

type referralClaimRequest struct {
	Code      string `json:"code" validate:"required,max=20"`
	VisitorID string `json:"visitorId" validate:"omitempty,max=64"`
}

func (h *ReferralHandler) Claim(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	var req referralClaimRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}
	res, err := h.svc.Claim(c.Context(), userID, req.Code, req.VisitorID)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral claim processed", res)
}

func (h *ReferralHandler) MyDashboard(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	d, err := h.svc.MyDashboard(c.Context(), userID)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral dashboard", d)
}

func (h *ReferralHandler) MyReferrals(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	codeID, ok := optionalCodeID(c)
	if !ok {
		return nil
	}
	f, err := attributionFilter(c)
	if err != nil {
		return err
	}
	page, err := h.svc.MyReferrals(c.Context(), userID, codeID, f)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referrals", page)
}

func (h *ReferralHandler) MyActivity(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	codeID, ok := optionalCodeID(c)
	if !ok {
		return nil
	}
	days, err := h.svc.MyActivity(c.Context(), userID, codeID, queryDays(c))
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral activity", days)
}

func (h *ReferralHandler) MyPayouts(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	p, err := h.svc.MyPayouts(c.Context(), userID)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral payouts", p)
}

func (h *ReferralHandler) AgentJoinInfo(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	info, err := h.svc.AgentJoinInfo(c.Context(), userID)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Agent signup", info)
}

type joinAgentRequest struct {
	AcceptTerms       bool   `json:"acceptTerms"`
	Phone             string `json:"phone" validate:"omitempty,max=32"`
	VerificationToken string `json:"verificationToken" validate:"omitempty,max=2048"`
}

func (h *ReferralHandler) JoinAsAgent(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	var req joinAgentRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}
	in := referral.JoinInput{AcceptTerms: req.AcceptTerms}
	if strings.TrimSpace(req.VerificationToken) != "" {
		if h.verifyPhone == nil {
			return response.Error(c, fiber.StatusServiceUnavailable, "Phone verification is not available", nil)
		}
		tokenPhone, err := h.verifyPhone(req.VerificationToken, "phone_verify")
		if err != nil {
			return validate.FailVerificationToken(c)
		}
		normalized, _ := services.NormalizePhone(req.Phone)
		if err := validate.FailPhoneVerifyToken(c, tokenPhone, normalized, req.Phone); err != nil {
			return err
		}
		in.VerifiedPhone = tokenPhone
	}
	_, ac := staffActor(c)
	d, err := h.svc.JoinAsAgent(c.Context(), userID, ac, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusCreated, "You are now an agent", d)
}

func (h *ReferralHandler) SetMyPayoutAccount(c fiber.Ctx) error {
	userID, ok := currentUser(c)
	if !ok {
		return nil
	}
	codeID, ok := optionalCodeID(c)
	if !ok {
		return nil
	}
	var in domain.ReferralPayoutAccount
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	_, ac := staffActor(c)
	p, err := h.svc.SetMyPayoutAccount(c.Context(), userID, codeID, ac, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Payout account saved", p)
}

// ---- Staff ----

func (h *ReferralHandler) GetSettings(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	st, err := h.svc.Settings(c.Context())
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral settings", st)
}

func (h *ReferralHandler) UpdateSettings(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	var in referral.SettingsInput
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	actor, ac := staffActor(c)
	st, err := h.svc.UpdateSettings(c.Context(), actor, ac, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral settings saved", st)
}

func staffActor(c fiber.Ctx) (uuid.UUID, *domain.AuditContext) {
	id, _ := uuid.Parse(GetUserID(c))
	ac := domain.AuditContext(GetAuditContext(c))
	return id, &ac
}

func (h *ReferralHandler) Sweep(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	res, err := h.svc.Sweep(c.Context())
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referrals rechecked", res)
}

func (h *ReferralHandler) ListPrograms(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	ps, err := h.svc.ListPrograms(c.Context())
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral programs", ps)
}

func (h *ReferralHandler) CreateProgram(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	var in referral.ProgramInput
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	actor, ac := staffActor(c)
	p, err := h.svc.CreateProgram(c.Context(), actor, ac, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusCreated, "Referral program created", p)
}

func (h *ReferralHandler) UpdateProgram(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	var in referral.ProgramInput
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	actor, ac := staffActor(c)
	p, err := h.svc.UpdateProgram(c.Context(), actor, ac, id, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral program updated", p)
}

func (h *ReferralHandler) ListCodes(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	page, limit, _, err := validate.BindPagination(c, 1, 20, 100)
	if err != nil {
		return err
	}
	res, err := h.svc.ListCodes(c.Context(), ports.ReferralCodeFilter{
		Kind: strings.TrimSpace(c.Query("kind")), Status: strings.TrimSpace(c.Query("status")),
		Search: strings.TrimSpace(c.Query("search")), Page: page, Limit: limit,
	})
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral codes", res)
}

func (h *ReferralHandler) CreateCode(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	var in referral.CodeInput
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	actor, ac := staffActor(c)
	d, err := h.svc.CreateCode(c.Context(), actor, ac, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusCreated, "Referral code created", d)
}

func (h *ReferralHandler) GetCode(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	d, err := h.svc.CodeDetail(c.Context(), id)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral code", d)
}

func (h *ReferralHandler) UpdateCode(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	var in referral.CodeInput
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	actor, ac := staffActor(c)
	d, err := h.svc.UpdateCode(c.Context(), actor, ac, id, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral code updated", d)
}

func (h *ReferralHandler) CodeReferrals(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	f, err := attributionFilter(c)
	if err != nil {
		return err
	}
	page, err := h.svc.CodeReferrals(c.Context(), id, f)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referrals", page)
}

func (h *ReferralHandler) CodeActivity(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	days, err := h.svc.CodeActivity(c.Context(), id, queryDays(c))
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral activity", days)
}

func (h *ReferralHandler) CodePayouts(c fiber.Ctx) error {
	if !requireStaffReader(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	p, err := h.svc.CodePayouts(c.Context(), id)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral payouts", p)
}

func (h *ReferralHandler) AddPayout(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	var in referral.PayoutInput
	if err := validate.BindAndValidate(c, &in); err != nil {
		return err
	}
	actor, ac := staffActor(c)
	p, err := h.svc.AddPayout(c.Context(), actor, ac, id, in)
	if err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusCreated, "Payout recorded", p)
}

type referralRejectRequest struct {
	Note string `json:"note" validate:"omitempty,max=255"`
}

func (h *ReferralHandler) Reject(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	var req referralRejectRequest
	if len(c.Body()) > 0 {
		if err := validate.BindAndValidate(c, &req); err != nil {
			return err
		}
	}
	actor, ac := staffActor(c)
	if err := h.svc.RejectReferral(c.Context(), actor, ac, id, req.Note); err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral rejected", nil)
}

func (h *ReferralHandler) Restore(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actor, ac := staffActor(c)
	if err := h.svc.RestoreReferral(c.Context(), actor, ac, id); err != nil {
		return referralError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Referral restored", nil)
}
