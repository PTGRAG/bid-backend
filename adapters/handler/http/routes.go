package http

import (
	"log/slog"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/adapters/notification/sms"
	"github.com/bid-hub/admin-backend/adapters/payment"
	"github.com/bid-hub/admin-backend/adapters/repository/postgres"
	"github.com/bid-hub/admin-backend/adapters/repository/redis"
	"github.com/bid-hub/admin-backend/adapters/service"
	r2storage "github.com/bid-hub/admin-backend/adapters/storage/r2"
	telegramadapter "github.com/bid-hub/admin-backend/adapters/telegram"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/core/services/notifications"
	"github.com/bid-hub/admin-backend/core/services/paymentledger"
	"github.com/bid-hub/admin-backend/platform/cache"
	"github.com/bid-hub/admin-backend/platform/env"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func RegisterRoutes(app *fiber.App, db *gorm.DB, redisClient *goredis.Client, cfg *env.Config) error {
	healthHandler := func(c fiber.Ctx) error {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"status":    "healthy",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	}
	app.Get("/health", healthHandler)
	app.Get("/api/health", healthHandler)

	api := app.Group("/api", CSRF(cfg))

	var limiterStorage fiber.Storage
	limiterConfig := limiter.Config{
		Max:        1000,
		Expiration: 1 * time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			return "limit:global:" + c.IP()
		},
	}
	if redisClient != nil {
		limiterStorage = cache.NewRedisStorage(redisClient)
		limiterConfig.Storage = limiterStorage
	}
	app.Use(limiter.New(limiterConfig))

	api.Get("/csrf-token", func(c fiber.Ctx) error { return HandleGetCSRFToken(c, cfg) })

	// Repositories
	userRepo := postgres.NewUserRepository(db)
	participantUserRepo := postgres.NewParticipantUserRepository(db)
	notificationRepo := postgres.NewNotificationRepository(db)
	cacheRepo := redis.NewCacheRepository(redisClient)

	// Services
	auditRepo := postgres.NewAuditLogRepository(db)
	auditService := services.NewAuditLogService(auditRepo)
	emailService, err := service.NewResendEmailService(cfg)
	if err != nil {
		return err
	}
	metricsProvider := NewPrometheusMetrics()
	baseURL := cfg.ChapaBaseURL
	if strings.TrimSpace(cfg.ChapaAPIURL) != "" {
		baseURL = strings.TrimSpace(cfg.ChapaAPIURL)
	}
	chapaClient := payment.NewChapaClient(cfg.ChapaSecretKey, baseURL)
	paymentLedgerRepo := postgres.NewPaymentLedgerRepository(db)
	refundRepo := postgres.NewRefundRecordRepository(db)
	legalDocRepo := postgres.NewLegalDocumentRepository(db)

	app.Use(metricsProvider.RequestMetrics())
	app.Get("/metrics", PrometheusHandler)

	smsService := sms.NewAfroMessageService(cfg)

	// Auction (Lowest Unique Bid) setup
	auctionRepo := postgres.NewAuctionRepository(db)
	participationRepo := postgres.NewAuctionParticipationRepository(db)
	bidRepo := postgres.NewBidRepository(db)
	notificationService := notifications.NewService(notificationRepo, userRepo, participantUserRepo, participationRepo, cacheRepo, emailService, smsService)
	telegramPoster := telegramadapter.NewChannelPoster(cfg, slog.Default())
	notificationService.SetTelegramMessenger(telegramPoster)
	auctionService := lub.NewService(auctionRepo, participationRepo, bidRepo, auditService, notificationService, telegramPoster)
	chapaPaymentSvc := chapapayment.NewService(paymentLedgerRepo, auctionService, participantUserRepo, notificationService, slog.Default())
	chapaPaymentSvc.SetChapaClient(chapaClient)
	chapaPaymentSvc.SetMetrics(metricsProvider)
	chapaPaymentSvc.SetAdminNotifier(telegramPoster)
	auctionAdminHandler := NewAuctionAdminHandler(auctionService)
	auctionHandler := NewAuctionHandler(auctionService, chapaPaymentSvc, chapaClient, userRepo, cfg)

	authService := services.NewAuthService(userRepo, participantUserRepo, auditService, emailService, cacheRepo, smsService, cfg)
	userService := services.NewUserService(userRepo, auditService, emailService, authService, cfg)
	paymentLedgerSvc := paymentledger.NewService(paymentLedgerRepo, refundRepo, chapaClient, auctionService, auditService)
	paymentLedgerSvc.SetMetrics(metricsProvider)
	paymentLedgerSvc.SetAdminNotifier(telegramPoster)
	dashboardService := services.NewDashboardService(
		userRepo, auctionRepo, participantUserRepo, auditRepo,
		paymentLedgerRepo, participationRepo, bidRepo,
	)
	legalDocService := services.NewLegalDocumentService(legalDocRepo, notificationService)

	// Handlers
	authHandler := NewAuthHandler(authService, cfg)
	userHandler := NewUserHandler(userService)
	participantHandler := NewParticipantHandler(participantUserRepo)
	dashboardHandler := NewDashboardHandler(dashboardService)
	notificationHandler := NewNotificationHandler(notificationService)
	legalDocHandler := NewLegalDocumentHandler(legalDocService)
	paymentHandler := NewPaymentHandler(paymentLedgerSvc)
	telegramBroadcastHandler := NewTelegramBroadcastHandler(telegramPoster, userRepo, participantUserRepo)

	r2PresignService, err := r2storage.NewPresignService(cfg)
	if err != nil {
		slog.Warn("R2 presign service not configured; media presign endpoints will return 503", "error", err)
		r2PresignService = nil
	}
	mediaPresignHandler := NewMediaPresignHandler(r2PresignService, cfg.MaxUploadSizeMB)

	registerPublicRoutes(api, legalDocHandler, authHandler, limiterStorage, cfg)
	registerAuthRoutes(api, authHandler, limiterStorage)

	optionalAuth := OptionalAuthMiddleware(authService, cfg.JWTAccessSecret, cacheRepo)
	registerPublicAuctionRoutes(api, optionalAuth, auctionHandler)

	protected := api.Group("/", AuthMiddlewareWithCache(authService, cfg.JWTAccessSecret, cacheRepo))

	registerUserRoutes(protected, userHandler, participantHandler, auctionHandler)
	registerPaymentRoutes(protected, paymentHandler)
	registerAdminAuctionRoutes(protected, auctionAdminHandler)
	registerMiscRoutes(protected, dashboardHandler, mediaPresignHandler, notificationHandler, legalDocHandler)
	registerTelegramBroadcastRoutes(api, optionalAuth, protected, telegramBroadcastHandler)

	return nil
}

func registerTelegramBroadcastRoutes(
	api fiber.Router,
	optionalAuth fiber.Handler,
	protected fiber.Router,
	h *TelegramBroadcastHandler,
) {
	api.Post("/webhooks/telegram", h.Webhook)
	// Mini App may share contact before the user has a JWT session.
	api.Post("/telegram/share-contact", optionalAuth, h.ShareContact)
	tg := protected.Group("/telegram")
	tg.Get("/broadcast/stats", h.Stats)
	tg.Post("/broadcast", h.Broadcast)
}

func registerPublicRoutes(api fiber.Router, legalDocHandler *LegalDocumentHandler, authHandler *AuthHandler, limiterStorage fiber.Storage, cfg *env.Config) {
	api.Get("/public/legal/:docType", legalDocHandler.PublicGet)
	api.Get("/public/auth-config", authHandler.PublicAuthConfig)
}

func registerAuthRoutes(api fiber.Router, authHandler *AuthHandler, limiterStorage fiber.Storage) {
	auth := api.Group("/auth")
	auth.Use(AuthRateLimit(limiterStorage))
	auth.Post("/login", authHandler.Login)
	auth.Post("/google", authHandler.GoogleLogin)
	auth.Post("/telegram", authHandler.TelegramLogin)
	auth.Post("/signup", authHandler.Signup)
	auth.Post("/send-otp", authHandler.SendOTP)
	auth.Post("/verify-otp", authHandler.VerifyOTP)
	auth.Post("/refresh", authHandler.Refresh)
	auth.Post("/logout", authHandler.Logout)
	auth.Post("/set-password", authHandler.SetPassword)
	auth.Post("/forgot-password", authHandler.ForgotPassword)
	auth.Post("/forgot-password/send-otp", authHandler.ForgotPasswordSendOTP)
	auth.Post("/forgot-password/verify-otp", authHandler.ForgotPasswordVerifyOTP)
	auth.Post("/forgot-password/reset", authHandler.ForgotPasswordReset)
	auth.Post("/reset-password", authHandler.SetPassword)
	auth.Post("/reset-password-otp", authHandler.ResetPasswordOTP)
}

func registerUserRoutes(protected fiber.Router, userHandler *UserHandler, participantHandler *ParticipantHandler, auctionHandler *AuctionHandler) {
	protected.Get("/me", userHandler.GetMe)
	protected.Patch("/me", userHandler.UpdateMe)
	protected.Patch("/me/password", userHandler.ChangeMyPassword)
	protected.Get("/me/participations", auctionHandler.ListMyParticipations)

	protected.Get("/users", userHandler.List)
	users := protected.Group("/users")
	users.Post("/", userHandler.Create)
	users.Get("/:id", userHandler.GetByID)
	users.Patch("/:id/status", userHandler.UpdateStatus)
	users.Delete("/:id", userHandler.Delete)

	protected.Get("/participants", participantHandler.List)
	protected.Patch("/participants/:id/status", participantHandler.UpdateStatus)
	protected.Delete("/participants/:id", participantHandler.Delete)
}

func registerPaymentRoutes(protected fiber.Router, paymentHandler *PaymentHandler) {
	payments := protected.Group("/payments")
	payments.Get("/", paymentHandler.List)
	payments.Post("/:txRef/refund", paymentHandler.Refund)
}

func registerMiscRoutes(protected fiber.Router, dashboardHandler *DashboardHandler, mediaPresignHandler *MediaPresignHandler, notificationHandler *NotificationHandler, legalDocHandler *LegalDocumentHandler) {
	audit := protected.Group("/audit-logs")
	audit.Get("/", dashboardHandler.ListAuditLogs)

	protected.Get("/stats/charts", dashboardHandler.GetCharts)
	protected.Get("/stats", dashboardHandler.GetStats)

	protected.Get("/legal-documents/:docType", legalDocHandler.StaffGet)
	protected.Put("/legal-documents/:docType", legalDocHandler.StaffPut)

	protected.Post("/media/presign", mediaPresignHandler.Presign)
	protected.Post("/media/presign-batch", mediaPresignHandler.PresignBatch)
	protected.Post("/media/presign-view-batch", mediaPresignHandler.PresignViewBatch)
	protected.Post("/media/upload", mediaPresignHandler.UploadMedia)

	notifications := protected.Group("/notifications")
	notifications.Get("/", notificationHandler.GetUserNotifications)
	notifications.Get("/unread-count", notificationHandler.GetUnreadCount)
	notifications.Get("/unread/count", notificationHandler.GetUnreadCount)
	notifications.Patch("/:id/read", notificationHandler.MarkAsRead)
	notifications.Patch("/read-all", notificationHandler.MarkAllAsRead)
	notifications.Delete("/:id", notificationHandler.DeleteNotification)
}

func registerPublicAuctionRoutes(api fiber.Router, optionalAuth fiber.Handler, publicHandler *AuctionHandler) {
	api.Get("/public/auctions", optionalAuth, publicHandler.ListPublicAuctions)
	api.Get("/public/auctions/:id", optionalAuth, publicHandler.GetPublicAuction)
	api.Get("/public/auctions/:id/audit", publicHandler.GetAuctionAudit)
	api.Post("/public/auctions/:auctionId/participate", optionalAuth, publicHandler.RecordParticipation)
	api.Post("/public/bids", optionalAuth, publicHandler.PlaceBid)
	api.Patch("/public/bids/:id", optionalAuth, publicHandler.UpdateBid)
	api.Get("/public/auctions/:auctionId/my-bids", optionalAuth, publicHandler.GetMyBids)
	api.Get("/public/auctions/:auctionId/my-participation", optionalAuth, publicHandler.GetMyParticipation)
	api.Post("/public/payments/verify", optionalAuth, publicHandler.VerifyChapaPayment)
	api.Get("/public/payments/verify/:txRef", optionalAuth, publicHandler.VerifyChapaPayment)
	api.Post("/webhooks/chapa", publicHandler.ChapaWebhook)
	api.Get("/webhooks/chapa", publicHandler.ChapaWebhook)
}

func registerAdminAuctionRoutes(protected fiber.Router, adminHandler *AuctionAdminHandler) {
	protected.Get("/auctions", adminHandler.List)
	auctions := protected.Group("/auctions")
	auctions.Post("/", adminHandler.Create)
	auctions.Get("/:id", adminHandler.GetByID)
	auctions.Patch("/:id", adminHandler.Update)
	auctions.Post("/:id/activate", adminHandler.Activate)
	auctions.Post("/:id/postpone", adminHandler.Postpone)
	auctions.Post("/:id/close", adminHandler.Close)
	auctions.Post("/:id/cancel", adminHandler.Cancel)
	auctions.Post("/:id/execute-draw", adminHandler.ExecuteDraw)
	auctions.Get("/:id/participants", adminHandler.ListParticipants)
	auctions.Get("/:id/bids", adminHandler.ListBids)
}
