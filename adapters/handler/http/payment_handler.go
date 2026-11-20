package http

import (
	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services/paymentledger"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

type PaymentHandler struct {
	svc *paymentledger.Service
}

func NewPaymentHandler(svc *paymentledger.Service) *PaymentHandler {
	return &PaymentHandler{svc: svc}
}

func (h *PaymentHandler) requireStaff(c fiber.Ctx) bool {
	role := GetUserRole(c)
	if role != domain.RoleSuperAdmin && role != domain.RoleAdmin && role != domain.RoleCompliance {
		response.Error(c, fiber.StatusForbidden, "Forbidden: Staff access required", nil)
		return false
	}
	return true
}

// List GET /api/payments
func (h *PaymentHandler) List(c fiber.Ctx) error {
	if !h.requireStaff(c) {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Staff access required", nil)
	}
	page, limit, _, err := validate.BindPagination(c, 1, 20, 200)
	if err != nil {
		return err
	}
	var q struct {
		Status        string `query:"status"`
		Processed     string `query:"processed"`
		RetryDisabled string `query:"retryDisabled"`
		Search        string `query:"search"`
	}
	_ = c.Bind().Query(&q)

	items, total, err := h.svc.List(c.Context(), ports.PaymentListFilter{
		Status:        q.Status,
		Processed:     q.Processed,
		RetryDisabled: q.RetryDisabled,
		Search:        q.Search,
		Limit:         limit,
		Offset:        (page - 1) * limit,
	})
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list payments", err)
	}

	out := make([]fiber.Map, 0, len(items))
	for i := range items {
		p := &items[i]
		m := fiber.Map{
			"id":               p.ID,
			"txRef":            p.TxRef,
			"chapaReference":   p.ChapaReference,
			"amount":           p.Amount.String(),
			"status":           p.Status,
			"processed":        p.Processed,
			"retryDisabled":    p.RetryDisabled,
			"lastError":        p.LastError,
			"createdAt":        p.CreatedAt,
			"updatedAt":        p.UpdatedAt,
			"participantName":  p.ParticipantName,
			"participantEmail": p.ParticipantEmail,
			"auctionTitle":     p.AuctionTitle,
			"hasParticipation": p.HasParticipation,
			"refundStatus":     p.RefundStatus,
			"purpose":          p.Purpose,
			"canRefund":        paymentledger.RefundEligible(p),
		}
		if p.UserID != nil {
			m["userId"] = p.UserID.String()
		}
		if p.AuctionID != nil {
			m["auctionId"] = p.AuctionID.String()
		}
		if p.ParticipationID != nil {
			m["participationId"] = p.ParticipationID.String()
		}
		out = append(out, m)
	}

	return response.Success(c, fiber.StatusOK, "Payments retrieved", fiber.Map{
		"items": out,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}

// Refund POST /api/payments/:txRef/refund
func (h *PaymentHandler) Refund(c fiber.Ctx) error {
	if !h.requireStaff(c) {
		return response.Error(c, fiber.StatusForbidden, "Forbidden: Staff access required", nil)
	}
	txRef := c.Params("txRef")
	if txRef == "" {
		return response.Error(c, fiber.StatusBadRequest, "txRef is required", nil)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.Bind().Body(&body)

	actorID, err := uuid.Parse(GetUserID(c))
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", nil)
	}

	if err := h.svc.Refund(c.Context(), txRef, actorID, body.Reason); err != nil {
		code, _ := domain.MapDomainErrorToHTTP(err)
		return response.Error(c, code, err.Error(), err)
	}
	return response.Success(c, fiber.StatusOK, "Refund initiated", fiber.Map{"txRef": txRef})
}
