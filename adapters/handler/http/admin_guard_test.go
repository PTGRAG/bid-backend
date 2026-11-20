package http

import (
	"net/http/httptest"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/gofiber/fiber/v3"
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
