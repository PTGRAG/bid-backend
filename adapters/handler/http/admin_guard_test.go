package http

import (
	"net/http/httptest"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// A non-admin must get a 403 and the guarded action must not run.
func TestRequireAdminRole_StopsNonAdmins(t *testing.T) {
	cases := []struct {
		role    domain.UserRole
		wantRun bool
		want    int
	}{
		{domain.RoleParticipant, false, fiber.StatusForbidden},
		{domain.RoleAgent, false, fiber.StatusForbidden},
		{"", false, fiber.StatusForbidden},
		{domain.RoleAdmin, true, fiber.StatusOK},
		{domain.RoleSuperAdmin, true, fiber.StatusOK},
	}
	for _, tc := range cases {
		ran := false
		app := fiber.New()
		app.Post("/x", func(c fiber.Ctx) error {
			c.Locals("role", tc.role)
			if !requireAdminRole(c) {
				return nil
			}
			ran = true
			return c.SendString("done")
		})
		resp, err := app.Test(httptest.NewRequest("POST", "/x", nil))
		if err != nil {
			t.Fatal(err)
		}
		if ran != tc.wantRun || resp.StatusCode != tc.want {
			t.Errorf("role %q: ran=%v status=%d, want ran=%v status=%d", tc.role, ran, resp.StatusCode, tc.wantRun, tc.want)
		}
	}
}

func TestTelegramBroadcast_RequiresAdmin(t *testing.T) {
	h := &TelegramBroadcastHandler{}
	ran := false
	app := fiber.New()
	app.Post("/x", func(c fiber.Ctx) error {
		c.Locals("role", domain.RoleParticipant)
		if !h.requireStaffMutator(c) {
			return nil
		}
		ran = true
		return nil
	})
	resp, err := app.Test(httptest.NewRequest("POST", "/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if ran || resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("participant reached broadcast: ran=%v status=%d", ran, resp.StatusCode)
	}
}

// The staff "get auction" endpoint shows drafts and unpaid winners, so a participant must get a
// 403 before the service is touched (the nil service would panic if it were reached).
func TestAdminGetAuction_RequiresAdmin(t *testing.T) {
	h := &AuctionAdminHandler{}
	for _, role := range []domain.UserRole{domain.RoleParticipant, domain.RoleAgent, domain.RoleCompliance, ""} {
		app := fiber.New()
		app.Get("/auctions/:id", func(c fiber.Ctx) error {
			c.Locals("role", role)
			return h.GetByID(c)
		})
		resp, err := app.Test(httptest.NewRequest("GET", "/auctions/"+uuid.NewString(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusForbidden {
			t.Errorf("role %q: status=%d, want 403", role, resp.StatusCode)
		}
	}
}
