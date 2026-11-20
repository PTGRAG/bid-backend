package domain

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type UserRole string
type UserStatus string

const (
	RoleSuperAdmin  UserRole = "super_admin"
	RoleAdmin       UserRole = "admin"
	RoleCompliance  UserRole = "compliance"
	RoleAgent       UserRole = "agent"
	RoleParticipant UserRole = "participant"

	StatusActive    UserStatus = "active"
	StatusSuspended UserStatus = "suspended"
	StatusPending   UserStatus = "pending"
)

type User struct {
	ID       uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	FullName string    `gorm:"type:varchar(255);not null"                     json:"fullName"`
	Email    string    `gorm:"type:varchar(255);uniqueIndex:ux_users_email;not null" json:"email"`
	Password string    `gorm:"column:password_hash;not null"                  json:"-"`
	Phone    string    `gorm:"type:varchar(50);index:idx_users_phone"         json:"phone"`

	TelegramID       *int64 `gorm:"uniqueIndex:ux_users_telegram_id"          json:"telegramId,omitempty"`
	TelegramUsername string `gorm:"type:varchar(100);index:idx_users_tg_user" json:"telegramUsername,omitempty"`
	JoinedVia        string `gorm:"type:varchar(30);default:'web'"            json:"joinedVia"`

	// Opt-in result messages for auctions the user did not win. The in-app notification is
	// always sent; these add an email and/or a Telegram DM. Winners are always told everywhere.
	NotifyResultsEmail    bool `gorm:"not null;default:false" json:"notifyResultsEmail"`
	NotifyResultsTelegram bool `gorm:"not null;default:false" json:"notifyResultsTelegram"`

	Role      UserRole       `gorm:"type:varchar(20);not null;index:idx_users_role_status,priority:1" json:"role"`
	Status    UserStatus     `gorm:"type:user_status;not null;default:'active';index:idx_users_role_status,priority:2" json:"status"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
