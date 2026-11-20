package http

import (
	"errors"
	"strings"

	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

func parseRolesParam(param string) []domain.UserRole {
	parts := strings.Split(strings.TrimSpace(param), ",")
	valid := map[domain.UserRole]struct{}{
		domain.RoleSuperAdmin:  {},
		domain.RoleAdmin:       {},
		domain.RoleCompliance:  {},
		domain.RoleAgent:       {},
		domain.RoleParticipant: {},
	}
	var roles []domain.UserRole
	for _, p := range parts {
		r := domain.UserRole(strings.ToLower(strings.TrimSpace(p)))
		if _, ok := valid[r]; ok {
			roles = append(roles, r)
		}
	}
	return roles
}

type UserHandler struct {
	service *services.UserService
}

func NewUserHandler(service *services.UserService) *UserHandler {
	return &UserHandler{service: service}
}

func (h *UserHandler) GetMe(c fiber.Ctx) error {
	userIDStr, err := GetUserIDOrUnauthorized(c)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", err)
	}

	parsedID, _ := uuid.Parse(userIDStr)

	user, err := h.service.GetByID(parsedID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return response.Error(c, fiber.StatusUnauthorized, "User not found", nil)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to fetch profile", err)
	}

	return response.Success(c, fiber.StatusOK, "Profile fetched successfully", fiber.Map{
		"user": user,
	})
}

type UpdateMeRequest struct {
	FullName    string `json:"fullName" validate:"omitempty,min=2,max=120"`
	Phone       string `json:"phone" validate:"omitempty,e164"`
	DisplayName string `json:"displayName" validate:"omitempty,min=2,max=80"` // agents only
}

func (h *UserHandler) UpdateMe(c fiber.Ctx) error {
	userIDStr, err := GetUserIDOrUnauthorized(c)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", err)
	}
	userID, _ := uuid.Parse(userIDStr)

	var req UpdateMeRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	if strings.TrimSpace(req.FullName) != "" {
		auditCtx := domain.AuditContext(GetAuditContext(c))
		if err := h.service.UpdateProfile(c.Context(), userID, strings.TrimSpace(req.FullName), req.Phone, &auditCtx); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return response.Error(c, fiber.StatusNotFound, "User not found", nil)
			}
			return response.Error(c, fiber.StatusInternalServerError, "Failed to update profile", err)
		}
	} else {
		user, _ := h.service.GetByID(userID)
		if user != nil {
			auditCtx := domain.AuditContext(GetAuditContext(c))
			if err := h.service.UpdateProfile(c.Context(), userID, user.FullName, req.Phone, &auditCtx); err != nil {
				return response.Error(c, fiber.StatusInternalServerError, "Failed to update profile", err)
			}
		}
	}

	user, err := h.service.GetByID(userID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to fetch updated profile", err)
	}
	return response.Success(c, fiber.StatusOK, "Profile updated successfully", fiber.Map{
		"user": user,
	})
}

type UpdateNotificationPreferencesRequest struct {
	NotifyResultsEmail    *bool `json:"notifyResultsEmail"`
	NotifyResultsTelegram *bool `json:"notifyResultsTelegram"`
}

// UpdateMyNotificationPreferences lets a participant opt in to email / Telegram messages about
// auctions they did not win (the in-app notification is always sent).
func (h *UserHandler) UpdateMyNotificationPreferences(c fiber.Ctx) error {
	userIDStr, err := GetUserIDOrUnauthorized(c)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", err)
	}
	userID, _ := uuid.Parse(userIDStr)
	var req UpdateNotificationPreferencesRequest
	if err := c.Bind().JSON(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "Invalid request body", err)
	}
	auditCtx := domain.AuditContext(GetAuditContext(c))
	user, err := h.service.UpdateNotificationPreferences(userID, req.NotifyResultsEmail, req.NotifyResultsTelegram, &auditCtx)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "User not found", nil)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to update notification preferences", err)
	}
	return response.Success(c, fiber.StatusOK, "Notification preferences updated", fiber.Map{"user": user})
}

type ChangeMyPasswordRequest struct {
	CurrentPassword string `json:"currentPassword" validate:"required"`
	NewPassword     string `json:"newPassword"     validate:"required,min=4"`
}

func (h *UserHandler) ChangeMyPassword(c fiber.Ctx) error {
	userIDStr, err := GetUserIDOrUnauthorized(c)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", err)
	}
	userID, _ := uuid.Parse(userIDStr)

	var req ChangeMyPasswordRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.ChangePassword(userID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			return response.Error(c, fiber.StatusUnauthorized, "Current password is incorrect", nil)
		}
		var appErr *domain.AppError
		if errors.As(err, &appErr) {
			return response.Error(c, appErr.Code, appErr.Message, nil)
		}
		return response.Error(c, fiber.StatusBadRequest, "Failed to change password", err)
	}

	return response.Success(c, fiber.StatusOK, "Password changed successfully", nil)
}

type CreateUserRequest struct {
	FullName string          `json:"fullName" validate:"required,min=2,max=120"`
	Email    string          `json:"email" validate:"required,email"`
	Phone    string          `json:"phone" validate:"omitempty,e164"`
	Role     domain.UserRole `json:"role" validate:"required,oneof=admin compliance"` // staff roles only; super_admin/agent/participant rejected
}

func (h *UserHandler) Create(c fiber.Ctx) error {
	actorRole := GetUserRole(c)
	if actorRole == "" {
		return response.Error(c, fiber.StatusForbidden, "Forbidden", nil)
	}

	// Compliance officers cannot add users
	if actorRole == domain.RoleCompliance {
		return response.Error(c, fiber.StatusForbidden, "Compliance officers cannot add users", nil)
	}

	// Only super_admin and admin can create staff
	if actorRole != domain.RoleSuperAdmin && actorRole != domain.RoleAdmin {
		return response.Error(c, fiber.StatusForbidden, "Only super admin and admin can add staff", nil)
	}

	var req CreateUserRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	if actorRole == domain.RoleAdmin && req.Role != domain.RoleCompliance {
		return response.Error(c, fiber.StatusForbidden, "Admin can only add compliance officers", nil)
	}

	user := &domain.User{
		FullName: req.FullName,
		Email:    req.Email,
		Phone:    req.Phone,
		Role:     req.Role,
	}

	auditCtx := domain.AuditContext(GetAuditContext(c))
	if err := h.service.Create(c.Context(), user, &auditCtx); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return response.Error(c, fiber.StatusConflict, "Email already exists", nil)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to create user", err)
	}

	return response.Success(c, fiber.StatusCreated, "User created successfully", user)
}

type UpdateStatusRequest struct {
	Status domain.UserStatus `json:"status" validate:"required,oneof=active suspended pending"`
}

func (h *UserHandler) UpdateStatus(c fiber.Ctx) error {
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}

	actorRole := GetUserRole(c)
	if actorRole != domain.RoleSuperAdmin && actorRole != domain.RoleAdmin {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Admin access required", nil)
	}

	var req UpdateStatusRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	auditCtx := domain.AuditContext(GetAuditContext(c))
	// Audit: Hierarchical Protection
	targetUser, err := h.service.GetByID(id)
	if err == nil && targetUser.Role == domain.RoleSuperAdmin && actorRole != domain.RoleSuperAdmin {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Only SuperAdmins can update SuperAdmin status", nil)
	}

	if err := h.service.UpdateStatus(c.Context(), id, req.Status, &auditCtx); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "User not found", nil)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to update status", err)
	}

	return response.Success(c, fiber.StatusOK, "Status updated successfully", nil)
}

func (h *UserHandler) GetByID(c fiber.Ctx) error {
	role := GetUserRole(c)
	if role != domain.RoleAdmin && role != domain.RoleSuperAdmin && role != domain.RoleCompliance {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Staff access required", nil)
	}

	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}

	user, err := h.service.GetByID(id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "User not found", nil)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to get user", err)
	}

	return response.Success(c, fiber.StatusOK, "User found", user)
}

func (h *UserHandler) List(c fiber.Ctx) error {
	role := GetUserRole(c)
	if role != domain.RoleAdmin && role != domain.RoleSuperAdmin && role != domain.RoleCompliance {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Staff access required", nil)
	}

	page, limit, _, err := validate.BindPagination(c, 1, 10, 100)
	if err != nil {
		return err
	}
	rolesParam := c.Query("roles")
	status := strings.TrimSpace(strings.ToLower(c.Query("status"))) // optional: active, suspended, pending
	search := strings.TrimSpace(c.Query("search"))                  // optional: search by name or email

	if rolesParam != "" {
		roles := parseRolesParam(rolesParam)
		if len(roles) > 0 {
			users, total, err := h.service.ListByRolesWithFilter(roles, status, search, page, limit)
			if err != nil {
				return response.Error(c, fiber.StatusInternalServerError, "Failed to list users", err)
			}
			return response.Success(c, fiber.StatusOK, "Users retrieved", fiber.Map{
				"items": users,
				"total": total,
				"page":  page,
				"limit": limit,
			})
		}
	}

	// No roles filter: list all users (with optional status/search would need another repo method; keep simple)
	users, total, err := h.service.List(page, limit)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list users", err)
	}

	return response.Success(c, fiber.StatusOK, "Users retrieved", fiber.Map{
		"items": users,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}
func (h *UserHandler) Delete(c fiber.Ctx) error {
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}

	actorRole := GetUserRole(c)
	if actorRole != domain.RoleSuperAdmin && actorRole != domain.RoleAdmin {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Admin access required", nil)
	}

	auditCtx := domain.AuditContext(GetAuditContext(c))
	// Audit: Hierarchical Protection (AI Security Expert)
	targetUser, err := h.service.GetByID(id)
	if err == nil && targetUser.Role == domain.RoleSuperAdmin && actorRole != domain.RoleSuperAdmin {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Only SuperAdmins can delete other SuperAdmins", nil)
	}

	if err := h.service.Delete(c.Context(), id, &auditCtx); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return response.Error(c, fiber.StatusNotFound, "User not found", nil)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to delete user", err)
	}

	return response.Success(c, fiber.StatusOK, "User deleted successfully", nil)
}
