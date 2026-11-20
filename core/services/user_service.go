package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/platform/env"
	"github.com/bid-hub/admin-backend/platform/logger"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type UserService struct {
	repo   ports.UserRepository
	audit  ports.AuditLogService
	email  ports.EmailService
	auth   *AuthService
	config *env.Config
}

func NewUserService(repo ports.UserRepository, audit ports.AuditLogService, email ports.EmailService, auth *AuthService, cfg *env.Config) *UserService {
	return &UserService{repo: repo, audit: audit, email: email, auth: auth, config: cfg}
}

func (s *UserService) Create(ctx context.Context, user *domain.User, auditCtx *domain.AuditContext) error {
	log := logger.FromContext(ctx)
	// Check if email exists (including soft-deleted records)
	existing, _ := s.repo.GetByEmailIncludeDeleted(user.Email)
	if existing != nil {
		return domain.ErrConflict
	}

	isStaff := user.Role == domain.RoleAdmin || user.Role == domain.RoleCompliance

	if isStaff {
		// For staff, we don't use manual password. Use a dummy random one and set status to pending.
		user.Password = "TEMP_PWD_" + uuid.New().String()
		user.Status = domain.StatusPending
	}

	// Hash password
	hashed, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user.Password = string(hashed)

	if err := s.repo.Create(user); err != nil {
		return err
	}

	// If staff, generate token and send email
	if isStaff {
		token, err := s.auth.GenerateSetPasswordToken(user)
		if err != nil {
			log.Error("user.set_password_token_failed", "email", maskEmail(user.Email), "error", err)
		} else {
			baseURL := strings.TrimSuffix(s.config.ParticipantAppURL, "/")
			if baseURL == "" {
				baseURL = "http://localhost:3000"
			}
			passwordSetupLink := fmt.Sprintf("%s/set-password?token=%s", baseURL, token)

			// Send the email with complete data
			emailErr := s.email.SendTemplateEmail(
				[]string{user.Email},
				fmt.Sprintf("Set Your Password - %s", s.config.AppName),
				"set_password",
				map[string]interface{}{
					"name":  user.FullName,
					"role":  user.Role,
					"token": token,
					"link":  passwordSetupLink,
				},
			)
			if emailErr != nil {
				log.Error("user.set_password_email_failed", "email", maskEmail(user.Email), "error", emailErr)
			} else {
				log.Info("user.set_password_email_sent", "email", maskEmail(user.Email))
			}
		}
	}

	// Log Action
	s.audit.Log(&domain.AuditLog{
		Action:   "user.created",
		Entity:   "user",
		EntityID: user.ID.String(),
		NewValue: string(user.Role),
	}, auditCtx)

	return nil
}

func (s *UserService) GetByID(id uuid.UUID) (*domain.User, error) {
	return s.repo.GetByID(id)
}

func (s *UserService) List(page, limit int) ([]domain.User, int64, error) {
	return s.repo.List(page, limit)
}

func (s *UserService) ListByRoles(roles []domain.UserRole, page, limit int) ([]domain.User, int64, error) {
	return s.repo.ListByRoles(roles, page, limit)
}

func (s *UserService) ListByRolesWithFilter(roles []domain.UserRole, status, search string, page, limit int) ([]domain.User, int64, error) {
	return s.repo.ListByRolesWithFilter(roles, status, search, page, limit)
}

func (s *UserService) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.UserStatus, auditCtx *domain.AuditContext) error {
	_ = ctx // reserved for downstream ctx propagation
	user, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	oldStatus := user.Status
	user.Status = status
	if err := s.repo.Update(user); err != nil {
		return err
	}

	// Log Action
	s.audit.Log(&domain.AuditLog{
		Action:   "user.status_changed",
		Entity:   "user",
		EntityID: id.String(),
		OldValue: string(oldStatus),
		NewValue: string(status),
	}, auditCtx)

	return nil
}

func (s *UserService) Delete(ctx context.Context, id uuid.UUID, auditCtx *domain.AuditContext) error {
	_ = ctx // reserved for downstream ctx propagation
	user, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}

	if err := s.repo.Delete(id); err != nil {
		return err
	}

	// Log Action
	s.audit.Log(&domain.AuditLog{
		Action:   "user.deleted",
		Entity:   "user",
		EntityID: id.String(),
		OldValue: string(user.Role) + " (" + user.Email + ")",
	}, auditCtx)

	return nil
}

// UpdateProfile updates the current user's fullName and phone (own profile only).
// UpdateProfile changes the user's name and/or phone. An empty value leaves that field as it is:
// the phone is how staff reach a winner, so a partial update must never wipe it.
func (s *UserService) UpdateProfile(ctx context.Context, userID uuid.UUID, fullName, phone string, auditCtx *domain.AuditContext) error {
	_ = ctx // reserved for downstream ctx propagation
	user, err := s.repo.GetByID(userID)
	if err != nil {
		return err
	}
	if name := strings.TrimSpace(fullName); name != "" {
		user.FullName = name
	}
	if p := strings.TrimSpace(phone); p != "" {
		user.Phone = p
	}
	if err := s.repo.Update(user); err != nil {
		return err
	}
	s.audit.Log(&domain.AuditLog{
		UserID:   userID,
		Action:   "user.profile.updated",
		Entity:   "user",
		EntityID: userID.String(),
	}, auditCtx)
	return nil
}

// UpdateNotificationPreferences sets the opt-in result messages (email / Telegram DM) for
// auctions the user did not win. A nil value leaves that setting unchanged.
func (s *UserService) UpdateNotificationPreferences(userID uuid.UUID, resultsEmail, resultsTelegram *bool, auditCtx *domain.AuditContext) (*domain.User, error) {
	user, err := s.repo.GetByID(userID)
	if err != nil {
		return nil, err
	}
	if resultsEmail != nil {
		user.NotifyResultsEmail = *resultsEmail
	}
	if resultsTelegram != nil {
		user.NotifyResultsTelegram = *resultsTelegram
	}
	if err := s.repo.Update(user); err != nil {
		return nil, err
	}
	s.audit.Log(&domain.AuditLog{
		UserID:   userID,
		Action:   "user.notification_preferences.updated",
		Entity:   "user",
		EntityID: userID.String(),
		NewValue: fmt.Sprintf("results_email=%t results_telegram=%t", user.NotifyResultsEmail, user.NotifyResultsTelegram),
	}, auditCtx)
	return user, nil
}

func (s *UserService) Update(user *domain.User) error {
	return s.repo.Update(user)
}

func (s *UserService) ChangePassword(userID uuid.UUID, currentPassword, newPassword string) error {
	user, err := s.repo.GetByID(userID)
	if err != nil {
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(currentPassword)); err != nil {
		return domain.ErrUnauthorized
	}
	if len(newPassword) < 4 {
		return domain.NewAppError(400, "password must be at least 4 characters", nil)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user.Password = string(hash)
	return s.repo.Update(user)
}