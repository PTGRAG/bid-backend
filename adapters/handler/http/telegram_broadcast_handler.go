package http

import (
	"errors"
	"strings"

	"github.com/bid-hub/admin-backend/adapters/handler/response"
	telegramadapter "github.com/bid-hub/admin-backend/adapters/telegram"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/gofiber/fiber/v3"
)

type TelegramBroadcastHandler struct {
	poster       *telegramadapter.ChannelPoster
	users        ports.UserRepository
	participants ports.ParticipantUserRepository
}

func NewTelegramBroadcastHandler(
	poster *telegramadapter.ChannelPoster,
	users ports.UserRepository,
	participants ...ports.ParticipantUserRepository,
) *TelegramBroadcastHandler {
	var partRepo ports.ParticipantUserRepository
	if len(participants) > 0 {
		partRepo = participants[0]
	}
	return &TelegramBroadcastHandler{
		poster:       poster,
		users:        users,
		participants: partRepo,
	}
}

type broadcastRequest struct {
	Target     string `json:"target" validate:"required,oneof=channel bot admin"`
	Text       string `json:"text"`
	MediaType  string `json:"mediaType" validate:"omitempty,oneof=none photo video animation"`
	FileID     string `json:"fileId"`
	ButtonText string `json:"buttonText"`
	ButtonURL  string `json:"buttonUrl"`
}

// requireStaffMutator writes a 403 and returns false when the caller is not an admin.
// Callers must stop when it returns false.
func (h *TelegramBroadcastHandler) requireStaffMutator(c fiber.Ctx) bool {
	role := GetUserRole(c)
	if role != domain.RoleSuperAdmin && role != domain.RoleAdmin {
		_ = response.Error(c, fiber.StatusForbidden, "Forbidden: Admin access required", nil)
		return false
	}
	return true
}

func (h *TelegramBroadcastHandler) Stats(c fiber.Ctx) error {
	if !h.requireStaffMutator(c) {
		return nil
	}
	userIDs, err := h.users.ListTelegramIDs()
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to count telegram users", err)
	}
	return response.Success(c, fiber.StatusOK, "Telegram broadcast stats", fiber.Map{
		"botAudience":     len(userIDs),
		"botConfigured":   h.poster != nil && h.poster.BroadcastConfigured(),
		"adminConfigured": h.poster != nil && h.poster.AdminConfigured(),
	})
}

func (h *TelegramBroadcastHandler) Broadcast(c fiber.Ctx) error {
	if !h.requireStaffMutator(c) {
		return nil
	}
	if h.poster == nil || !h.poster.BroadcastConfigured() {
		return response.Error(c, fiber.StatusServiceUnavailable, "Telegram bot is not configured", nil)
	}

	var req broadcastRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	in := telegramadapter.BroadcastInput{
		Target:     req.Target,
		Text:       req.Text,
		MediaType:  req.MediaType,
		FileID:     req.FileID,
		ButtonText: req.ButtonText,
		ButtonURL:  req.ButtonURL,
	}

	var chatIDs []int64
	if strings.EqualFold(req.Target, telegramadapter.TargetBot) {
		ids, err := h.users.ListTelegramIDs()
		if err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "Failed to load telegram users", err)
		}
		chatIDs = ids
	}

	result, err := h.poster.Broadcast(c.Context(), in, chatIDs)
	if err != nil && (result == nil || result.Sent == 0) {
		return response.Error(c, fiber.StatusBadRequest, err.Error(), err)
	}
	msg := "Broadcast completed"
	if err != nil {
		msg = "Broadcast finished with errors"
	}
	return response.Success(c, fiber.StatusOK, msg, result)
}

type shareContactRequest struct {
	TelegramID int64  `json:"telegramId" validate:"required,gt=0"`
	Phone      string `json:"phone" validate:"required"`
	FullName   string `json:"fullName"`
	Username   string `json:"username"`
}

func (h *TelegramBroadcastHandler) syncParticipantUser(user *domain.User) {
	if h.participants == nil || user == nil || strings.TrimSpace(user.Phone) == "" {
		return
	}
	_ = h.participants.UpsertByPhone(&domain.ParticipantUser{
		ID:        user.ID.String(),
		Email:     user.Email,
		FullName:  user.FullName,
		Phone:     user.Phone,
		Status:    "active",
		CreatedAt: user.CreatedAt,
		UpdatedAt: user.UpdatedAt,
	})
}

// ShareContact registers (or links) a Mini App contact onto users:
// new phone → create user; existing web phone → attach telegram_id + username.
func (h *TelegramBroadcastHandler) ShareContact(c fiber.Ctx) error {
	var req shareContactRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}
	normalized, _ := services.NormalizePhone(req.Phone)
	if normalized == "" {
		normalized = strings.TrimSpace(req.Phone)
	}

	user, err := h.users.UpsertTelegramProfile(
		req.TelegramID,
		req.Username,
		req.FullName,
		normalized,
	)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return response.Error(c, fiber.StatusConflict, "This phone is already linked to another Telegram account", err)
		}
		if errors.Is(err, domain.ErrInvalidInput) {
			return response.Error(c, fiber.StatusBadRequest, "Invalid Telegram contact", err)
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to save contact", err)
	}
	h.syncParticipantUser(user)

	return response.Success(c, fiber.StatusOK, "Contact saved", fiber.Map{
		"telegramId": req.TelegramID,
		"phone":      normalized,
		"userId":     user.ID,
		"joinedVia":  user.JoinedVia,
	})
}

// TelegramWebhook records /start and shared contacts onto the users table.
func (h *TelegramBroadcastHandler) Webhook(c fiber.Ctx) error {
	var update struct {
		Message *struct {
			Text    string `json:"text"`
			Contact *struct {
				PhoneNumber string `json:"phone_number"`
				FirstName   string `json:"first_name"`
				LastName    string `json:"last_name"`
				UserID      int64  `json:"user_id"`
			} `json:"contact"`
			Chat *struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
			From *struct {
				ID        int64  `json:"id"`
				Username  string `json:"username"`
				FirstName string `json:"first_name"`
				LastName  string `json:"last_name"`
			} `json:"from"`
		} `json:"message"`
	}
	if err := c.Bind().Body(&update); err != nil {
		return c.SendStatus(fiber.StatusOK)
	}
	msg := update.Message
	if msg == nil || msg.From == nil || msg.From.ID == 0 {
		return c.SendStatus(fiber.StatusOK)
	}
	if msg.Chat != nil && msg.Chat.Type != "" && msg.Chat.Type != "private" {
		return c.SendStatus(fiber.StatusOK)
	}

	name := strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
	phone := ""
	sharedContact := false
	if msg.Contact != nil && strings.TrimSpace(msg.Contact.PhoneNumber) != "" {
		sharedContact = true
		phone, _ = services.NormalizePhone(msg.Contact.PhoneNumber)
		if phone == "" {
			phone = strings.TrimSpace(msg.Contact.PhoneNumber)
		}
		if cn := strings.TrimSpace(msg.Contact.FirstName + " " + msg.Contact.LastName); cn != "" {
			name = cn
		}
	}

	user, _ := h.users.UpsertTelegramProfile(msg.From.ID, msg.From.Username, name, phone)
	h.syncParticipantUser(user)

	chatID := msg.From.ID
	if msg.Chat != nil && msg.Chat.ID != 0 {
		chatID = msg.Chat.ID
	}

	if h.poster != nil {
		if sharedContact {
			_ = h.poster.SendContactThanks(c.Context(), chatID)
		} else if strings.HasPrefix(strings.TrimSpace(msg.Text), "/start") {
			_ = h.poster.SendStartWelcome(c.Context(), chatID)
		}
	}

	return c.SendStatus(fiber.StatusOK)
}
