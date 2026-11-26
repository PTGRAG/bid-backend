package http

import (
	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// GetMyFreeBids returns the signed-in customer's near-miss free bids.
func (h *AuctionHandler) GetMyFreeBids(c fiber.Ctx) error {
	participantID := GetUserID(c)
	if _, err := uuid.Parse(participantID); err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Authentication required", nil)
	}
	out, err := h.svc.FreeBidBalance(c.Context(), participantID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to load free bids", err)
	}
	return response.Success(c, fiber.StatusOK, "Free bids retrieved", out)
}

// GetNearMissSettings returns the near-miss free-bid rules (staff).
func (h *AuctionAdminHandler) GetNearMissSettings(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	s, err := h.svc.NearMissSettings(c.Context())
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to load near-miss settings", err)
	}
	return response.Success(c, fiber.StatusOK, "Near-miss settings retrieved", fiber.Map{
		"settings": s,
		"defaults": domain.DefaultNearMissSettings(),
		"limits": fiber.Map{
			"maxRunnerUps":  domain.MaxNearMissRunnerUps,
			"maxDuplicates": domain.MaxNearMissDuplicates,
		},
	})
}

type nearMissSettingsRequest struct {
	Enabled           bool `json:"enabled"`
	RunnerUps         int  `json:"runnerUps"`
	IncludeDuplicates bool `json:"includeDuplicates"`
	MaxDuplicates     int  `json:"maxDuplicates"`
}

// UpdateNearMissSettings saves the near-miss free-bid rules (staff).
func (h *AuctionAdminHandler) UpdateNearMissSettings(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	var req nearMissSettingsRequest
	if err := c.Bind().JSON(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "Invalid request body", err)
	}
	actor, _ := uuid.Parse(GetUserID(c))
	s, err := h.svc.UpdateNearMissSettings(c.Context(), domain.NearMissSettings{
		Enabled:           req.Enabled,
		RunnerUps:         req.RunnerUps,
		IncludeDuplicates: req.IncludeDuplicates,
		MaxDuplicates:     req.MaxDuplicates,
	}, actor)
	if err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Near-miss settings saved", fiber.Map{"settings": s})
}

// ListAuctionFreeBids lists the free bids given for an auction (staff).
func (h *AuctionAdminHandler) ListAuctionFreeBids(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	items, err := h.svc.ListNearMissGrants(c.Context(), id)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list free bids", err)
	}
	if items == nil {
		items = []domain.FreeBidGrant{}
	}
	return response.Success(c, fiber.StatusOK, "Free bids retrieved", fiber.Map{"items": items})
}

// CreditAuctionFreeBids (re)runs the near-miss credit for an auction whose winner paid (staff).
// Customers already credited are skipped, so it is safe to press twice.
func (h *AuctionAdminHandler) CreditAuctionFreeBids(c fiber.Ctx) error {
	if !requireAdminRole(c) {
		return nil
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actor, _ := uuid.Parse(GetUserID(c))
	created, err := h.svc.CreditNearMisses(c.Context(), id, actor)
	if err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Near-miss free bids credited", fiber.Map{"credited": len(created)})
}
