package services

import (
	"context"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
)

type profileUserRepo struct {
	ports.UserRepository
	user *domain.User
}

func (r *profileUserRepo) GetByID(uuid.UUID) (*domain.User, error) {
	cp := *r.user
	return &cp, nil
}

func (r *profileUserRepo) Update(u *domain.User) error {
	r.user = u
	return nil
}

type noopAudit struct{}

func (noopAudit) Log(*domain.AuditLog, *domain.AuditContext) error { return nil }

func TestUpdateProfile_KeepsFieldsLeftEmpty(t *testing.T) {
	cases := []struct {
		name, fullName, phone, wantName, wantPhone string
	}{
		{"name only keeps phone", "New Name", "", "New Name", "+251911000000"},
		{"blank phone keeps phone", "New Name", "   ", "New Name", "+251911000000"},
		{"phone only keeps name", "", "+251922000000", "Old Name", "+251922000000"},
		{"both change", "New Name", "+251922000000", "New Name", "+251922000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &profileUserRepo{user: &domain.User{ID: uuid.New(), FullName: "Old Name", Phone: "+251911000000"}}
			s := &UserService{repo: repo, audit: noopAudit{}}
			if err := s.UpdateProfile(context.Background(), repo.user.ID, tc.fullName, tc.phone, &domain.AuditContext{}); err != nil {
				t.Fatal(err)
			}
			if repo.user.FullName != tc.wantName || repo.user.Phone != tc.wantPhone {
				t.Errorf("got name=%q phone=%q, want name=%q phone=%q", repo.user.FullName, repo.user.Phone, tc.wantName, tc.wantPhone)
			}
		})
	}
}
