package http

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// Staff referral endpoints must stop participants and agents before the service is touched
// (the nil service would panic if reached). Reads also allow compliance; changes need an admin.
func TestReferralStaffRoutes_RequireStaff(t *testing.T) {
	h := &ReferralHandler{}
	id := uuid.NewString()
	routes := []struct {
		method, path, pattern string
		handler               fiber.Handler
		complianceAllowed     bool
	}{
		{"GET", "/referral-codes", "/referral-codes", h.ListCodes, true},
		{"GET", "/referral-codes/" + id, "/referral-codes/:id", h.GetCode, true},
		{"GET", "/referral-codes/" + id + "/referrals", "/referral-codes/:id/referrals", h.CodeReferrals, true},
		{"GET", "/referral-programs", "/referral-programs", h.ListPrograms, true},
		{"POST", "/referral-codes", "/referral-codes", h.CreateCode, false},
		{"PATCH", "/referral-codes/" + id, "/referral-codes/:id", h.UpdateCode, false},
		{"POST", "/referral-codes/" + id + "/payouts", "/referral-codes/:id/payouts", h.AddPayout, false},
		{"PATCH", "/referral-programs/" + id, "/referral-programs/:id", h.UpdateProgram, false},
		{"POST", "/referral-programs", "/referral-programs", h.CreateProgram, false},
		{"POST", "/referral-attributions/" + id + "/reject", "/referral-attributions/:id/reject", h.Reject, false},
		{"POST", "/referrals/sweep", "/referrals/sweep", h.Sweep, false},
	}
	roles := []domain.UserRole{domain.RoleParticipant, domain.RoleAgent, "", domain.RoleCompliance}
	for _, r := range routes {
		for _, role := range roles {
			if role == domain.RoleCompliance && r.complianceAllowed {
				continue
			}
			app := fiber.New()
			handler := r.handler
			app.Add([]string{r.method}, r.pattern, func(c fiber.Ctx) error {
				c.Locals("role", role)
				return handler(c)
			})
			req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != fiber.StatusForbidden {
				t.Errorf("%s %s as %q: status %d, want 403", r.method, r.path, role, resp.StatusCode)
			}
		}
	}
}
