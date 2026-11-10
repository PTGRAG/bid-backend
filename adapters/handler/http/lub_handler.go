package http

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/platform/env"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// AuctionHandler handles participant-facing auction endpoints.
type AuctionHandler struct {
	svc             *lub.Service
	chapaPaymentSvc *chapapayment.Service
	chapaClient     ports.PaymentProvider
	userRepo        ports.UserRepository
	cfg             *env.Config
}

func NewAuctionHandler(
	svc *lub.Service,
	chapaPaymentSvc *chapapayment.Service,
	chapaClient ports.PaymentProvider,
	userRepo ports.UserRepository,
	cfg *env.Config,
) *AuctionHandler {
	return &AuctionHandler{
		svc:             svc,
		chapaPaymentSvc: chapaPaymentSvc,
		chapaClient:     chapaClient,
		userRepo:        userRepo,
		cfg:             cfg,
	}
}

type RecordParticipationRequest struct {
	AuctionID     string    `json:"auctionId"     validate:"omitempty,uuid"`
	ProductID     string    `json:"productId"     validate:"omitempty,uuid"`
	ParticipantID string    `json:"participantId"`
	TransactionID string    `json:"transactionId"`
	FeePaid       float64   `json:"feePaid"`
	BidValue      float64   `json:"bidValue"`
	BidValues     []float64 `json:"bidValues"`
	CheckoutURL   string    `json:"checkoutUrl"`
	ReturnURL     string    `json:"returnUrl"`
	// ExpectEntranceFree is true when the UI quoted the customer a waived entrance fee. If the
	// server can no longer honour it, checkout fails with 409 instead of charging more.
	ExpectEntranceFree bool `json:"expectEntranceFree"`
}

const maxBidPackSize = 10

func normalizeBidPack(req RecordParticipationRequest) ([]domain.Money, error) {
	raw := make([]float64, 0, len(req.BidValues)+1)
	if len(req.BidValues) > 0 {
		raw = append(raw, req.BidValues...)
	} else if req.BidValue > 0 {
		raw = append(raw, req.BidValue)
	}
	if len(raw) == 0 {
		return nil, domain.NewAppError(400, "Add at least one bid value", nil)
	}
	if len(raw) > maxBidPackSize {
		return nil, domain.NewAppError(400, fmt.Sprintf("Maximum %d bids per payment", maxBidPackSize), nil)
	}

	out := make([]domain.Money, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, v := range raw {
		if v <= 0 {
			return nil, domain.NewAppError(400, "Each bid must be greater than zero", nil)
		}
		m := domain.NewMoney(v)
		key := m.String()
		if _, ok := seen[key]; ok {
			return nil, domain.NewAppError(400, "Bid values in a pack must be unique", nil)
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	return out, nil
}

type PlaceBidRequest struct {
	AuctionID     string  `json:"auctionId"     validate:"omitempty,uuid"`
	ProductID     string  `json:"productId"     validate:"omitempty,uuid"`
	ParticipantID string  `json:"participantId"`
	BidValue      float64 `json:"bidValue"      validate:"required,gt=0"`
}

type UpdateBidRequest struct {
	NewBidValue float64 `json:"newBidValue" validate:"required,gt=0"`
}

func (h *AuctionHandler) ListPublicAuctions(c fiber.Ctx) error {
	page, limit, offset, err := validate.BindPagination(c, 1, 12, 100)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(c.Query("status"))
	search := strings.TrimSpace(c.Query("search"))
	sortBy := strings.TrimSpace(c.Query("sort"))
	viewerID := strings.TrimSpace(GetUserID(c))

	cards, total, statusCounts, err := h.svc.ListPublicAuctionCardsFiltered(ports.PublicAuctionFilter{
		Status: status,
		Search: search,
		Sort:   sortBy,
		Limit:  limit,
		Offset: offset,
	}, viewerID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list auctions", err)
	}
	totalPages := int64(0)
	if limit > 0 && total > 0 {
		totalPages = (total + int64(limit) - 1) / int64(limit)
	}
	hasMore := int64(page) < totalPages

	return response.Success(c, fiber.StatusOK, "Auctions retrieved", fiber.Map{
		"items":        cards,
		"total":        total,
		"page":         page,
		"limit":        limit,
		"totalPages":   totalPages,
		"hasMore":      hasMore,
		"statusCounts": statusCounts,
	})
}

func (h *AuctionHandler) GetPublicAuction(c fiber.Ctx) error {
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	a, err := h.svc.GetAuctionByID(id)
	if err != nil {
		return mapLUBError(c, err)
	}
	if a.Status != domain.AuctionStatusExecuted {
		a.WinnerBidID = nil
		a.WinnerParticipantID = nil
		a.WinnerBidValue = nil
		a.WinnerName = ""
	}
	viewerID := strings.TrimSpace(GetUserID(c))
	if viewerID != "" {
		if part, _ := h.svc.GetParticipation(id, viewerID); part != nil && part.Status == domain.ParticipationStatusActive {
			a.UserHasJoined = true
		}
	}
	h.svc.EnrichAuctionFreeEntrance(a, viewerID)
	return response.Success(c, fiber.StatusOK, "Auction found", a)
}

func (h *AuctionHandler) GetAuctionAudit(c fiber.Ctx) error {
	id, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	audit, err := h.svc.GetAuctionAudit(id)
	if err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Auction audit retrieved", audit)
}

func formatChapaPhone(raw string) string {
	digits := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(raw), "+", ""), " ", "")
	if strings.HasPrefix(digits, "251") && len(digits) == 12 {
		digits = "0" + digits[3:]
	}
	if len(digits) == 10 && (strings.HasPrefix(digits, "09") || strings.HasPrefix(digits, "07")) {
		return digits
	}
	return ""
}

// chapaPublicCallbackURL reports whether Chapa will accept this as callback_url.
// Chapa rejects loopback hosts with 400 "callback url must be a valid URL".
func chapaPublicCallbackURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		return false
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	return true
}

// resolveChapaCallbackURL prefers CALLBACK_URL (production), then a public request host.
// Loopback is never sent to Chapa — that only affects local dev; production must set
// CALLBACK_URL=https://<public-api>/api/webhooks/chapa so webhooks always fire.
func resolveChapaCallbackURL(cfgCallback string, reqScheme, reqHost string) string {
	if u := strings.TrimSpace(cfgCallback); chapaPublicCallbackURL(u) {
		return u
	}
	scheme := strings.TrimSpace(reqScheme)
	if scheme == "" {
		scheme = "https"
	}
	host := strings.TrimSpace(reqHost)
	if host == "" {
		return ""
	}
	derived := fmt.Sprintf("%s://%s/api/webhooks/chapa", scheme, host)
	if chapaPublicCallbackURL(derived) {
		return derived
	}
	return ""
}

func (h *AuctionHandler) RecordParticipation(c fiber.Ctx) error {
	var req RecordParticipationRequest
	_ = c.Bind().JSON(&req)

	rawID := strings.TrimSpace(req.AuctionID)
	if rawID == "" {
		rawID = strings.TrimSpace(req.ProductID)
	}
	if rawID == "" {
		rawID = strings.TrimSpace(c.Params("auctionId"))
	}
	if rawID == "" {
		rawID = strings.TrimSpace(c.Params("productId"))
	}
	auctionID, err := uuid.Parse(rawID)
	if err != nil || auctionID == uuid.Nil {
		return response.Error(c, fiber.StatusBadRequest, "valid auctionId is required", nil)
	}

	participantID := strings.TrimSpace(GetUserID(c))
	if participantID == "" {
		participantID = strings.TrimSpace(req.ParticipantID)
	}
	userUUID, err := uuid.Parse(participantID)
	if err != nil || userUUID == uuid.Nil {
		return response.Error(c, fiber.StatusUnauthorized, "Authentication required", nil)
	}

	bidPack, err := normalizeBidPack(req)
	if err != nil {
		return mapLUBError(c, err)
	}

	var a *domain.Auction
	for _, bidMoney := range bidPack {
		validated, vErr := h.svc.ValidateAuctionAndBid(auctionID, participantID, bidMoney)
		if vErr != nil {
			return mapLUBError(c, vErr)
		}
		a = validated
	}
	if a == nil {
		return response.Error(c, fiber.StatusBadRequest, "Invalid bid pack", nil)
	}

	if h.chapaClient == nil || h.chapaPaymentSvc == nil {
		return response.Error(c, fiber.StatusServiceUnavailable, "Chapa payment gateway is not configured", nil)
	}

	// Free-entrance promo (admin-controlled per auction): when this customer qualifies, the
	// entrance fee of their first bid is waived; the remaining bids pay fee + bid through Chapa. The server
	// alone decides; the client only says what total it showed the customer.
	_, waive, fErr := h.svc.FreeEntranceStatus(auctionID, participantID)
	if fErr != nil {
		return mapLUBError(c, fErr)
	}
	if waive {
		// Resolve any earlier in-flight free checkout of this customer first (it may turn out
		// to be paid, which also changes eligibility), then re-check.
		if sErr := h.chapaPaymentSvc.SettleStaleWaivedCheckouts(c.Context(), userUUID); sErr != nil {
			return mapLUBError(c, sErr)
		}
		_, waive, fErr = h.svc.FreeEntranceStatus(auctionID, participantID)
		if fErr != nil {
			return mapLUBError(c, fErr)
		}
	}
	if req.ExpectEntranceFree && !waive {
		return mapLUBError(c, domain.ErrFreeEntranceUnavailable)
	}

	txRef := "R-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:20]

	// Free entrance waives the entrance fee of the FIRST bid only (index 0); every other bid in
	// the pack still pays fee + bid. Always computed here, never taken from the client.
	chargeAmount := domain.CheckoutCharge(bidPack, a.EntranceFee, waive)

	if waive {
		// Atomically reserves the free slot and records the pending checkout.
		if err := h.chapaPaymentSvc.CreateWaivedPendingPayment(c.Context(), txRef, userUUID, auctionID, chargeAmount, bidPack...); err != nil {
			if errors.Is(err, domain.ErrFreeEntranceUnavailable) {
				// Lost the slot to a concurrent joiner: never silently charge the full fee.
				return mapLUBError(c, err)
			}
			return response.Error(c, fiber.StatusInternalServerError, "Failed to create pending payment", err)
		}
	} else if err := h.chapaPaymentSvc.CreatePendingPayment(c.Context(), txRef, userUUID, auctionID, chargeAmount, bidPack...); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to create pending payment", err)
	}

	email := "bidder@carramarket.com"
	firstName := "CarraMarket"
	lastName := "Bidder"
	phone := ""

	if h.userRepo != nil {
		if u, uErr := h.userRepo.GetByID(userUUID); uErr == nil && u != nil {
			if strings.Contains(u.Email, "@") {
				email = strings.TrimSpace(u.Email)
			}
			parts := strings.Fields(strings.TrimSpace(u.FullName))
			if len(parts) > 0 {
				firstName = parts[0]
				if len(parts) > 1 {
					lastName = strings.Join(parts[1:], " ")
				}
			}
			phone = formatChapaPhone(u.Phone)
		}
	}

	cfgCallback := ""
	if h.cfg != nil {
		cfgCallback = h.cfg.CallbackURL
	}
	scheme := c.Protocol()
	if proto := strings.TrimSpace(c.Get("X-Forwarded-Proto")); proto != "" {
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	}
	callbackURL := resolveChapaCallbackURL(cfgCallback, scheme, c.Host())

	returnBase := strings.TrimSpace(req.ReturnURL)
	if returnBase == "" {
		returnBase = strings.TrimSpace(req.CheckoutURL)
	}
	if returnBase == "" {
		origin := strings.TrimSpace(c.Get("Origin"))
		if origin == "" && h.cfg != nil {
			origin = strings.TrimSpace(h.cfg.ParticipantAppURL)
		}
		if origin == "" {
			origin = "https://carramarket.com"
		}
		returnBase = fmt.Sprintf("%s/auctions/%s", strings.TrimRight(origin, "/"), auctionID.String())
	}

	bidValueStrings := make([]string, 0, len(bidPack))
	for _, m := range bidPack {
		bidValueStrings = append(bidValueStrings, m.String())
	}

	parsedReturn, uErr := url.Parse(returnBase)
	if uErr == nil {
		q := parsedReturn.Query()
		q.Set("tx_ref", txRef)
		if len(bidValueStrings) == 1 {
			q.Set("bid_value", bidValueStrings[0])
		} else if len(bidValueStrings) > 1 {
			q.Set("bid_values", strings.Join(bidValueStrings, "_"))
		}
		parsedReturn.RawQuery = q.Encode()
		returnBase = parsedReturn.String()
	}

	meta := map[string]interface{}{
		"auction_id":   auctionID.String(),
		"user_id":      userUUID.String(),
		"ticket_count": strconv.Itoa(len(bidPack)),
	}
	if len(bidValueStrings) == 1 {
		meta["bid_value"] = bidValueStrings[0]
	}
	if len(bidValueStrings) > 0 {
		// Store as pipe-separated string so Chapa's flat string meta map accepts it without 400 validation errors
		meta["bid_values"] = strings.Join(bidValueStrings, "_")
	}

	desc := "Auction entrance fee and bid"
	if waive {
		// Informational only; the waiver is read from the ledger row, never from this meta.
		meta["entrance_waived"] = "true"
		desc = "Auction entrance fee and bid (first entrance fee free)"
	}
	if len(bidPack) > 1 {
		desc = fmt.Sprintf("Auction bid pack %d tickets", len(bidPack))
		if waive {
			desc = fmt.Sprintf("Auction bid pack %d tickets (first entrance fee free)", len(bidPack))
		}
	}

	initResp, err := h.chapaClient.InitializeTransaction(ports.ChapaInitializeRequest{
		Amount:      chargeAmount.String(),
		Currency:    "ETB",
		Email:       email,
		FirstName:   firstName,
		LastName:    lastName,
		PhoneNumber: phone,
		TxRef:       txRef,
		CallbackURL: callbackURL,
		ReturnURL:   returnBase,
		Title:       "CarraMarket",
		Description: desc,
		Meta:        meta,
	})
	if err != nil {
		if waive {
			// The checkout never reached Chapa: give the reserved free slot back right away.
			h.chapaPaymentSvc.AbortWaivedCheckout(c.Context(), txRef, "chapa initialize failed")
		}
		return response.Error(c, fiber.StatusBadGateway, "Could not initialize Chapa payment: "+err.Error(), err)
	}

	return response.Success(c, fiber.StatusOK, "Chapa checkout initialized", fiber.Map{
		"checkoutUrl":    initResp.CheckoutURL,
		"txRef":          initResp.TxRef,
		"ticketCount":    len(bidPack),
		"amount":         chargeAmount,
		"entranceWaived": waive,
	})
}

func (h *AuctionHandler) VerifyChapaPayment(c fiber.Ctx) error {
	txRef := strings.TrimSpace(c.Params("txRef"))
	if txRef == "" {
		txRef = strings.TrimSpace(c.Query("tx_ref"))
	}
	if txRef == "" {
		txRef = strings.TrimSpace(c.Query("trx_ref"))
	}
	var body struct {
		TxRef     string    `json:"txRef"`
		TxRefAlt  string    `json:"tx_ref"`
		BidValue  float64   `json:"bidValue"`
		BidValues []float64 `json:"bidValues"`
	}
	_ = c.Bind().JSON(&body)
	if txRef == "" {
		txRef = strings.TrimSpace(body.TxRef)
	}
	if txRef == "" {
		txRef = strings.TrimSpace(body.TxRefAlt)
	}
	if txRef == "" {
		return response.Error(c, fiber.StatusBadRequest, "txRef is required", nil)
	}

	bidVals := make([]domain.Money, 0)
	for _, v := range body.BidValues {
		if v > 0 {
			bidVals = append(bidVals, domain.NewMoney(v))
		}
	}
	if len(bidVals) == 0 && body.BidValue > 0 {
		bidVals = append(bidVals, domain.NewMoney(body.BidValue))
	}
	if len(bidVals) == 0 {
		if qVals := strings.TrimSpace(c.Query("bid_values")); qVals != "" {
			for _, part := range strings.FieldsFunc(qVals, func(r rune) bool {
				return r == ',' || r == '_' || r == ';' || r == '|'
			}) {
				if parsed, pErr := strconv.ParseFloat(strings.TrimSpace(part), 64); pErr == nil && parsed > 0 {
					bidVals = append(bidVals, domain.NewMoney(parsed))
				}
			}
		}
	}
	if len(bidVals) == 0 {
		if qVal := strings.TrimSpace(c.Query("bid_value")); qVal != "" {
			if parsed, pErr := strconv.ParseFloat(qVal, 64); pErr == nil && parsed > 0 {
				bidVals = append(bidVals, domain.NewMoney(parsed))
			}
		}
	}

	if h.chapaClient == nil || h.chapaPaymentSvc == nil {
		return response.Error(c, fiber.StatusServiceUnavailable, "Chapa payment gateway is not configured", nil)
	}

	// Fast path: webhook/reconciler already recorded success — do not block the UI on Chapa.
	if existing, eErr := h.chapaPaymentSvc.GetPaymentByTxRef(c.Context(), txRef); eErr == nil && existing != nil &&
		existing.Status == domain.PaymentStatusSuccess {
		if !existing.Processed {
			_, _ = h.chapaPaymentSvc.RetryPaymentByTxRef(c.Context(), txRef)
		}
		h.chapaPaymentSvc.EnsureBidsForPayment(c.Context(), txRef, bidVals)
		return h.paymentVerifyResponse(c, txRef, true, domain.PaymentStatusSuccess)
	}

	snap, err := h.chapaClient.VerifyTransaction(txRef)
	if err != nil {
		return response.Error(c, fiber.StatusBadGateway, "Failed to verify Chapa transaction: "+err.Error(), err)
	}

	metaMap := chapapayment.NormalizeChapaMeta(snap.Meta)
	verifiedSnap := &chapapayment.VerifiedSnapshot{
		TxRef:          txRef,
		ChapaReference: snap.Reference,
		APIStatus:      "success",
		DataStatus:     snap.Status,
		Amount:         snap.Amount,
		Meta:           metaMap,
		CustomerEmail:  snap.CustomerEmail,
		CustomerFirst:  snap.CustomerFirst,
		BidValues:      bidVals,
	}
	if len(bidVals) == 1 {
		verifiedSnap.BidValue = bidVals[0]
	}

	if err := h.chapaPaymentSvc.ProcessVerified(c.Context(), verifiedSnap); err != nil {
		if errors.Is(err, chapapayment.ErrFulfillmentDeferred) {
			// Payment captured; background retry will finish participation/bids.
			return response.Success(c, fiber.StatusAccepted, "Payment verified; fulfillment pending retry", fiber.Map{
				"verified": true,
				"status":   snap.Status,
				"txRef":    txRef,
				"pending":  true,
			})
		}
		return response.Error(c, fiber.StatusInternalServerError, "Failed to process verified payment", err)
	}

	return h.paymentVerifyResponse(c, txRef, strings.EqualFold(snap.Status, "success"), snap.Status)
}

func (h *AuctionHandler) paymentVerifyResponse(c fiber.Ctx, txRef string, verified bool, status string) error {
	out := fiber.Map{
		"verified": verified,
		"status":   status,
		"txRef":    txRef,
	}
	if p, pErr := h.chapaPaymentSvc.GetPaymentByTxRef(c.Context(), txRef); pErr == nil && p != nil && p.AuctionID != nil && p.UserID != nil {
		part, _ := h.svc.GetParticipation(*p.AuctionID, p.UserID.String())
		bids, _ := h.svc.ListMyBids(*p.AuctionID, p.UserID.String())
		out["auctionId"] = p.AuctionID.String()
		out["participating"] = part != nil && part.Status == domain.ParticipationStatusActive
		out["participation"] = part
		out["bids"] = bids
	}
	return response.Success(c, fiber.StatusOK, "Payment verification complete", out)
}

func (h *AuctionHandler) ChapaWebhook(c fiber.Ctx) error {
	txRef := strings.TrimSpace(c.Query("tx_ref"))
	if txRef == "" {
		txRef = strings.TrimSpace(c.Query("trx_ref"))
	}
	var payload map[string]interface{}
	_ = c.Bind().JSON(&payload)
	if txRef == "" && payload != nil {
		if v, ok := payload["tx_ref"].(string); ok {
			txRef = strings.TrimSpace(v)
		}
		if txRef == "" {
			if v, ok := payload["trx_ref"].(string); ok {
				txRef = strings.TrimSpace(v)
			}
		}
	}
	if txRef == "" {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"status": "ignored"})
	}

	if h.chapaClient == nil || h.chapaPaymentSvc == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "unavailable"})
	}

	snap, err := h.chapaClient.VerifyTransaction(txRef)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"status": "verify_failed"})
	}
	metaMap := chapapayment.NormalizeChapaMeta(snap.Meta)
	verifiedSnap := &chapapayment.VerifiedSnapshot{
		TxRef:          txRef,
		ChapaReference: snap.Reference,
		APIStatus:      "success",
		DataStatus:     snap.Status,
		Amount:         snap.Amount,
		Meta:           metaMap,
		CustomerEmail:  snap.CustomerEmail,
		CustomerFirst:  snap.CustomerFirst,
	}
	if err := h.chapaPaymentSvc.ProcessVerified(c.Context(), verifiedSnap); err != nil {
		// Ask Chapa to retry when fulfillment is deferred; identity/bids are durable now.
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"status": "retry"})
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"status": "ok"})
}

func (h *AuctionHandler) ListMyParticipations(c fiber.Ctx) error {
	userID, err := GetUserIDOrUnauthorized(c)
	if err != nil {
		return response.Error(c, fiber.StatusUnauthorized, "Unauthorized", err)
	}
	page, limit, _, err := validate.BindPagination(c, 1, 20, 100)
	if err != nil {
		return err
	}
	parts, total, err := h.svc.ListParticipantParticipations(userID, page, limit)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list participations", err)
	}
	items := make([]fiber.Map, 0, len(parts))
	for i := range parts {
		part := parts[i]
		auction, aErr := h.svc.GetAuctionByID(part.AuctionID)
		if aErr != nil || auction == nil {
			continue
		}
		if auction.Status != domain.AuctionStatusExecuted {
			auction.WinnerBidID = nil
			auction.WinnerParticipantID = nil
			auction.WinnerBidValue = nil
		}
		bids, _ := h.svc.ListMyBids(part.AuctionID, userID)
		items = append(items, fiber.Map{
			"auction":       auction,
			"participation": part,
			"bids":          bids,
		})
	}
	return response.Success(c, fiber.StatusOK, "Participations retrieved", fiber.Map{
		"items": items,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}

func (h *AuctionHandler) PlaceBid(c fiber.Ctx) error {
	var req PlaceBidRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}
	rawID := strings.TrimSpace(req.AuctionID)
	if rawID == "" {
		rawID = strings.TrimSpace(req.ProductID)
	}
	auctionID, err := uuid.Parse(rawID)
	if err != nil || auctionID == uuid.Nil {
		return response.Error(c, fiber.StatusBadRequest, "valid auctionId is required", nil)
	}

	participantID := strings.TrimSpace(req.ParticipantID)
	if participantID == "" {
		participantID = strings.TrimSpace(GetUserID(c))
	}
	if participantID == "" {
		return response.Error(c, fiber.StatusUnauthorized, "Authentication required", nil)
	}

	bid, err := h.svc.PlaceBid(
		c.Context(),
		auctionID,
		participantID,
		domain.NewMoney(req.BidValue),
	)
	if err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusCreated, "Bid placed", bid)
}

func (h *AuctionHandler) UpdateBid(c fiber.Ctx) error {
	bidID, err := PathParamUUID(c, "id", "id")
	if err != nil {
		return err
	}
	participantID := strings.TrimSpace(c.Query("participantId"))
	if participantID == "" {
		var body struct {
			ParticipantID string `json:"participantId"`
		}
		_ = c.Bind().JSON(&body)
		participantID = strings.TrimSpace(body.ParticipantID)
	}
	if participantID == "" {
		participantID = strings.TrimSpace(GetUserID(c))
	}
	if participantID == "" {
		return response.Error(c, fiber.StatusUnauthorized, "Authentication required", nil)
	}

	var req UpdateBidRequest
	if err := validate.BindAndValidate(c, &req); err != nil {
		return err
	}

	bid, err := h.svc.UpdateBid(c.Context(), bidID, participantID, domain.NewMoney(req.NewBidValue))
	if err != nil {
		return mapLUBError(c, err)
	}
	return response.Success(c, fiber.StatusOK, "Bid updated", bid)
}

func (h *AuctionHandler) GetMyBids(c fiber.Ctx) error {
	paramName := "auctionId"
	if c.Params("auctionId") == "" {
		paramName = "productId"
	}
	auctionID, err := PathParamUUID(c, paramName, paramName)
	if err != nil {
		return err
	}
	participantID := strings.TrimSpace(c.Query("participantId"))
	if participantID == "" {
		participantID = strings.TrimSpace(GetUserID(c))
	}
	if participantID == "" {
		return response.Success(c, fiber.StatusOK, "Bids retrieved", []domain.Bid{})
	}
	bids, err := h.svc.ListMyBids(auctionID, participantID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "Failed to list bids", err)
	}
	return response.Success(c, fiber.StatusOK, "Bids retrieved", bids)
}

func (h *AuctionHandler) GetMyParticipation(c fiber.Ctx) error {
	paramName := "auctionId"
	if c.Params("auctionId") == "" {
		paramName = "productId"
	}
	auctionID, err := PathParamUUID(c, paramName, paramName)
	if err != nil {
		return err
	}
	participantID := strings.TrimSpace(c.Query("participantId"))
	if participantID == "" {
		participantID = strings.TrimSpace(GetUserID(c))
	}
	if participantID == "" {
		return response.Success(c, fiber.StatusOK, "Not participating", fiber.Map{"participating": false})
	}
	part, err := h.svc.GetParticipation(auctionID, participantID)
	if err != nil {
		return mapLUBError(c, err)
	}
	if part == nil {
		return response.Success(c, fiber.StatusOK, "Not participating", fiber.Map{"participating": false})
	}
	return response.Success(c, fiber.StatusOK, "Participation found", fiber.Map{
		"participating": true,
		"participation": part,
	})
}
