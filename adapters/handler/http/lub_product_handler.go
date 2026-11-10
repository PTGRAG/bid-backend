package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// AuctionAdminHandler handles admin CRUD for auctions.
type AuctionAdminHandler struct {
	svc *lub.Service
}

func NewAuctionAdminHandler(svc *lub.Service) *AuctionAdminHandler {
	return &AuctionAdminHandler{svc: svc}
}

type CreateAuctionRequest struct {
	Title           string   `json:"title"           validate:"omitempty,min=2,max=255"`
	Name            string   `json:"name"            validate:"omitempty,min=2,max=255"`
	Description     string   `json:"description"     validate:"omitempty"`
	ImageURL        string   `json:"imageUrl"        validate:"omitempty,url"`
	ImageURLs       []string `json:"imageUrls"       validate:"omitempty,dive,url"`
	VideoURL        string   `json:"videoUrl"        validate:"omitempty,url"`
	EntranceFee     float64  `json:"entranceFee"     validate:"required,gt=0"`
	RetailValue     float64  `json:"retailValue"     validate:"omitempty,gte=0"`
	MinParticipants int      `json:"minParticipants" validate:"omitempty,gte=1"`
	MinBidValue     float64  `json:"minBidValue"     validate:"omitempty,gt=0"`
	MaxBidValue     *float64 `json:"maxBidValue"     validate:"omitempty,gt=0"`
	StartAt         *string  `json:"startAt"         validate:"omitempty"`
	CloseAt         *string  `json:"closeAt"         validate:"omitempty"`
	// Free-entrance promo. Omitted = off. Limit: number or "100" / "1k" / "all" (see
	// domain.ParseFreeEntranceLimit); omitted with the promo on = domain.DefaultFreeEntranceLimit.
	FreeEntranceEnabled *bool           `json:"freeEntranceEnabled"`
	FreeEntranceLimit   json.RawMessage `json:"freeEntranceLimit"`
}

type UpdateAuctionRequest struct {
	Title           string   `json:"title"           validate:"omitempty,min=2,max=255"`
	Name            string   `json:"name"            validate:"omitempty,min=2,max=255"`
	Description     string   `json:"description"     validate:"omitempty"`
	ImageURL        string   `json:"imageUrl"        validate:"omitempty"`
	ImageURLs       []string `json:"imageUrls"       validate:"omitempty"`
	VideoURL        string   `json:"videoUrl"        validate:"omitempty"`
	EntranceFee     float64  `json:"entranceFee"     validate:"omitempty,gt=0"`
	RetailValue     float64  `json:"retailValue"     validate:"omitempty,gte=0"`
	MinParticipants int      `json:"minParticipants" validate:"omitempty,gte=1"`
	MinBidValue     float64  `json:"minBidValue"     validate:"omitempty,gt=0"`
	MaxBidValue     *float64 `json:"maxBidValue"     validate:"omitempty,gt=0"`
	StartAt         *string  `json:"startAt"         validate:"omitempty"`
	CloseAt         *string  `json:"closeAt"         validate:"omitempty"`
	// Free-entrance promo. Omitted fields are left unchanged.
	FreeEntranceEnabled *bool           `json:"freeEntranceEnabled"`
	FreeEntranceLimit   json.RawMessage `json:"freeEntranceLimit"`
}

// parseFreeEntranceLimitField reads the admin "free entrance limit" value, which the staff
// text box sends as a JSON string ("100", "1k", "all") or a JSON number.
// provided=false when the field was omitted or null.
func parseFreeEntranceLimitField(raw json.RawMessage) (limit int, provided bool, err error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false, nil
	}
	var text string
	if strings.HasPrefix(trimmed, `"`) {
		if uErr := json.Unmarshal(raw, &text); uErr != nil {
			return 0, true, fmt.Errorf("free entrance limit must be text or a whole number: %w", domain.ErrInvalidInput)
		}
	} else {
		// A bare JSON number: only plain non-negative integers survive the strict parser
		// (rejects 1.5, 1e3, -5, ...).
		text = trimmed
	}
	limit, err = domain.ParseFreeEntranceLimit(text)
	return limit, true, err
}

func freeEntranceFieldError(c fiber.Ctx, err error) error {
	msg := err.Error()
	return validate.FailWithStatus(c, fiber.StatusBadRequest, msg, "BAD_REQUEST",
		validate.FieldError{Field: "freeEntranceLimit", Rule: "invalid", Message: msg})
}

func normalizeAuctionImages(cover string, urls []string) (string, []string) {
	seen := make(map[string]struct{}, len(urls)+1)
	out := make([]string, 0, len(urls)+1)
	add := func(raw string) {
		u := strings.TrimSpace(raw)
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	add(cover)
	for _, u := range urls {
		add(u)
	}
	if len(out) == 0 {
		return "", []string{}
	}
	return out[0], out
}

type PostponeAuctionRequest struct {
	NewCloseAt string `json:"newCloseAt" validate:"required"`
}

func (h *AuctionAdminHandler) Create(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	actorIDStr, err := GetUserIDOrUnauthorized(c)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", err)
	}
	actorID, _ := uuid.Parse(actorIDStr)

	var req CreateAuctionRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(req.Name)
	}
	if len(title) < 2 {
		return response.Error(c, fiber.StatusBadRequest, "title is required (min 2 chars)", nil)
	}

	freeLimit, freeLimitProvided, lErr := parseFreeEntranceLimitField(req.FreeEntranceLimit)
	if lErr != nil {
		return freeEntranceFieldError(c, lErr)
	}
	if !freeLimitProvided {
		freeLimit = domain.DefaultFreeEntranceLimit
	}

	cover, images := normalizeAuctionImages(req.ImageURL, req.ImageURLs)
	minParticipants := req.MinParticipants
	if minParticipants <= 0 {
		minParticipants = 10
	}
	a := &domain.Auction{
		Title:               title,
		Description:         req.Description,
		ImageURL:            cover,
		ImageURLs:           images,
		VideoURL:            req.VideoURL,
		EntranceFee:         domain.NewMoney(req.EntranceFee),
		RetailValue:         domain.NewMoney(req.RetailValue),
		MinParticipants:     minParticipants,
		MinBidValue:         domain.NewMoney(req.MinBidValue),
		FreeEntranceEnabled: req.FreeEntranceEnabled != nil && *req.FreeEntranceEnabled,
		FreeEntranceLimit:   freeLimit,
	}
	if req.MaxBidValue != nil {
		v := domain.NewMoney(*req.MaxBidValue)
		a.MaxBidValue = &v
	}
	if req.StartAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.StartAt); err == nil {
			a.StartAt = &t
		}
	}
	if req.CloseAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.CloseAt); err == nil {
			a.CloseAt = &t
		}
	}

	if err := h.svc.CreateAuction(c.Context(), a, actorID); err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusCreated, "Auction created", a)
}

func (h *AuctionAdminHandler) List(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	page, limit, _, err := validate.BindPagination(c, 1, 20, 100)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(c.Query("status"))
	search := strings.TrimSpace(c.Query("search"))

	auctions, total, err := h.svc.ListAuctions(status, search, page, limit)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list auctions", err)
	}
	return response.Success(c, fiber.StatusOK, "Auctions retrieved", fiber.Map{
		"items": auctions,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}

func (h *AuctionAdminHandler) GetByID(c fiber.Ctx) error {
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	a, err := h.svc.GetAuctionByID(id)
	if err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Auction found", a)
}

func (h *AuctionAdminHandler) Update(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actorIDStr, _ := GetUserIDOrUnauthorized(c)
	actorID, _ := uuid.Parse(actorIDStr)

	var req UpdateAuctionRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(req.Name)
	}

	freeLimit, freeLimitProvided, lErr := parseFreeEntranceLimitField(req.FreeEntranceLimit)
	if lErr != nil {
		return freeEntranceFieldError(c, lErr)
	}

	cover, images := normalizeAuctionImages(req.ImageURL, req.ImageURLs)
	updates := &domain.Auction{
		Title:           title,
		Description:     req.Description,
		ImageURL:        cover,
		ImageURLs:       images,
		VideoURL:        req.VideoURL,
		EntranceFee:     domain.NewMoney(req.EntranceFee),
		RetailValue:     domain.NewMoney(req.RetailValue),
		MinParticipants: req.MinParticipants,
		MinBidValue:     domain.NewMoney(req.MinBidValue),
	}
	if req.MaxBidValue != nil {
		v := domain.NewMoney(*req.MaxBidValue)
		updates.MaxBidValue = &v
	}
	if req.StartAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.StartAt); err == nil {
			updates.StartAt = &t
		}
	}
	if req.CloseAt != nil {
		if t, err := time.Parse(time.RFC3339, *req.CloseAt); err == nil {
			updates.CloseAt = &t
		}
	}

	var free *lub.FreeEntranceSettings
	if freeLimitProvided || req.FreeEntranceEnabled != nil {
		free = &lub.FreeEntranceSettings{Enabled: req.FreeEntranceEnabled}
		if freeLimitProvided {
			free.Limit = &freeLimit
		}
	}

	if err := h.svc.UpdateAuctionWithFreeEntrance(c.Context(), id, updates, free, actorID); err != nil {
		return mapLUBError(c, err)
	}
	a, _ := h.svc.GetAuctionByID(id)
	return response.Success(c, fiber.StatusOK, "Auction updated", a)
}

func (h *AuctionAdminHandler) Activate(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actorIDStr, _ := GetUserIDOrUnauthorized(c)
	actorID, _ := uuid.Parse(actorIDStr)

	if err := h.svc.ActivateAuction(c.Context(), id, actorID); err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Auction activated", nil)
}

func (h *AuctionAdminHandler) Postpone(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actorIDStr, _ := GetUserIDOrUnauthorized(c)
	actorID, _ := uuid.Parse(actorIDStr)

	var req PostponeAuctionRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}
	newCloseAt, parseErr := time.Parse(time.RFC3339, req.NewCloseAt)
	if parseErr != nil {
		return response.Error(c, fiber.StatusBadRequest, "Invalid newCloseAt format (use RFC3339)", parseErr)
	}
	if err := h.svc.PostponeAuction(c.Context(), id, newCloseAt, actorID); err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Auction postponed", nil)
}

func (h *AuctionAdminHandler) Close(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actorIDStr, _ := GetUserIDOrUnauthorized(c)
	actorID, _ := uuid.Parse(actorIDStr)

	if err := h.svc.CloseAuction(c.Context(), id, actorID); err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Auction closed", nil)
}

func (h *AuctionAdminHandler) Cancel(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actorIDStr, _ := GetUserIDOrUnauthorized(c)
	actorID, _ := uuid.Parse(actorIDStr)

	if err := h.svc.CancelAuction(c.Context(), id, actorID); err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Auction cancelled", nil)
}

func (h *AuctionAdminHandler) ExecuteDraw(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	actorIDStr, _ := GetUserIDOrUnauthorized(c)
	actorID, _ := uuid.Parse(actorIDStr)

	result, err := h.svc.ExecuteDraw(c.Context(), id, "manual", actorID)
	if err != nil {
		return mapLUBError(c, err)
	}
	if !result.HasWinner {
		return response.Success(c, fiber.StatusOK, "Draw executed: no unique bid found   all bid values were duplicated", result)
	}
	return response.Success(c, fiber.StatusOK, "Draw executed successfully", result)
}

func (h *AuctionAdminHandler) ListParticipants(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	page, limit, _, err := validate.BindPagination(c, 1, 20, 100)
	if err != nil {
		return err
	}
	parts, total, err := h.svc.ListParticipations(id, page, limit)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list participants", err)
	}
	return response.Success(c, fiber.StatusOK, "Participants retrieved", fiber.Map{
		"items": parts,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}

func (h *AuctionAdminHandler) ListBids(c fiber.Ctx) error {
	if err := requireAdminRole(c); err != nil {
		return err
	}
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	page, limit, _, err := validate.BindPagination(c, 1, 20, 200)
	if err != nil {
		return err
	}
	bids, total, err := h.svc.ListAuctionBids(id, page, limit)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list bids", err)
	}
	return response.Success(c, fiber.StatusOK, "Bids retrieved", fiber.Map{
		"items": bids,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}

func requireAdminRole(c fiber.Ctx) error {
	role := GetUserRole(c)
	if role != domain.RoleAdmin && role != domain.RoleSuperAdmin {
		return response.Error(c, fiber.StatusForbidden, "Admin access required", nil)
	}
	return nil
}

func mapLUBError(c fiber.Ctx, err error) error {
	status, _ := domain.MapDomainErrorToHTTP(err)
	msg := err.Error()
	var appErr *domain.AppError
	if errors.As(err, &appErr) {
		return response.Error(c, appErr.Code, appErr.Message, nil)
	}
	return response.Error(c, status, msg, err)
}
