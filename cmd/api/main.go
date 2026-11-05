package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	handler "github.com/bid-hub/admin-backend/adapters/handler/http"
	"github.com/bid-hub/admin-backend/adapters/handler/response"
	"github.com/bid-hub/admin-backend/adapters/payment"
	"github.com/bid-hub/admin-backend/adapters/repository/postgres"
	telegramadapter "github.com/bid-hub/admin-backend/adapters/telegram"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/services"
	"github.com/bid-hub/admin-backend/core/services/chapapayment"
	"github.com/bid-hub/admin-backend/core/services/lub"
	"github.com/bid-hub/admin-backend/core/services/notifications"
	"github.com/bid-hub/admin-backend/platform/cache"
	"github.com/bid-hub/admin-backend/platform/database"
	"github.com/bid-hub/admin-backend/platform/env"
	"github.com/bid-hub/admin-backend/platform/validate"
	"github.com/bid-hub/admin-backend/platform/worker"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	logger "github.com/gofiber/fiber/v3/middleware/logger"
	fiberRecover "github.com/gofiber/fiber/v3/middleware/recover"

	"gorm.io/gorm"
)

func main() {
	time.Local = time.UTC

	// 1. Load Configuration
	cfg := env.LoadConfig()

	// 2. Initialize Sentry
	if cfg.SentryDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			Environment:      cfg.SentryEnvironment,
			TracesSampleRate: cfg.SentryTracesSampleRate,
			EnableLogs:       true,

			BeforeSendLog: func(log *sentry.Log) *sentry.Log {
				if log.Severity <= sentry.LogSeverityInfo {
					return nil
				}
				return log
			},

			BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
				// Ignore health-check errors.
				if event.Request != nil {
					requestURL := event.Request.URL

					if requestURL == "/health" ||
						requestURL == "/api/health" {
						return nil
					}

					// Remove sensitive request headers.
					for key := range event.Request.Headers {
						switch strings.ToLower(key) {
						case "authorization",
							"cookie",
							"x-csrf-token":
							delete(event.Request.Headers, key)
						}
					}
				}

				return event
			},
		}); err != nil {
			slog.Warn(
				"Sentry initialization failed; continuing without error tracking",
				"error", err,
			)
		} else {
			slog.Info(
				"Sentry error tracking initialized",
				"environment", cfg.SentryEnvironment,
			)

			defer sentry.Flush(3 * time.Second)
		}
	} else {
		slog.Warn("SENTRY_DSN not set; error tracking disabled")
	}

	// 3. Validate Configuration
	if err := cfg.Validate(); err != nil {
		slog.Error("Configuration validation failed", "error", err)
		os.Exit(1)
	}

	// 4. Connect to Database
	db, err := database.Connect(cfg)
	if err != nil {
		slog.Error("Critical: Could not connect to database", "error", err)
		os.Exit(1)
	}

	// Apply safe additive schema patches.
	database.EnsureSafeSchema(db)

	if !isProductionEnvironment() {
		slog.Info("Development environment - running database migrations")
		database.Migrate(db)
	} else {
		slog.Info(
			"Production environment - applied safe schema patches; skipping full AutoMigrate/seed",
		)
	}

	// 5. Connect to Redis
	redisClient := cache.InitRedis(cfg)

	if redisClient != nil {
		defer redisClient.Close()
	}

	// 6. Initialize Fiber
	app := fiber.New(fiber.Config{
		BodyLimit: 100 * 1024 * 1024,

		ErrorHandler: func(c fiber.Ctx, err error) error {
			// Validation errors already have their response envelope.
			if validate.IsValidation(err) {
				return nil
			}

			code, errCode := domain.MapDomainErrorToHTTP(err)
			msg := err.Error()

			var fiberErr *fiber.Error

			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
				errCode = domain.MapHTTPToErrorCode(code)
				msg = fiberErr.Message
			}

			// Capture unexpected server errors.
			if code >= 500 && cfg.SentryDSN != "" {
				sentry.CaptureException(err)
			}

			// CORS headers are handled exclusively by CORS middleware.
			// Never reflect arbitrary Origin headers here.

			return response.Error(c, code, msg, errCode)
		},
	})

	// 7. Middleware

	// Recover must be first to catch panics from subsequent middleware/handlers.
	app.Use(fiberRecover.New(fiberRecover.Config{
		EnableStackTrace: false,

		StackTraceHandler: func(c fiber.Ctx, e any) {
			// Keep stack traces out of production logs.
		},
	}))

	app.Use(handler.RequestID())

	app.Use(logger.New(logger.Config{
		Format: `{"time":"${time}","status":${status},"method":"${method}","path":"${path}","latency":"${latency}","ip":"${ip}","user_agent":"${ua}","error":"${error}"}` + "\n",
	}))

	// 8. Sentry Middleware
	if cfg.SentryDSN != "" {
		app.Use(func(c fiber.Ctx) error {
			hub := sentry.CurrentHub().Clone()

			hub.Scope().SetTag(
				"transaction",
				c.Method()+" "+c.Path(),
			)

			hub.Scope().SetContext("Request", map[string]interface{}{
				"method": c.Method(),
				"path":   c.Path(),
				"ip":     c.IP(),
			})

			defer func() {
				if recovered := recover(); recovered != nil {
					eventID := hub.RecoverWithContext(
						c.Context(),
						recovered,
					)

					if eventID != nil {
						sentry.Flush(2 * time.Second)
					}

					// Let Fiber recover middleware handle the panic.
					panic(recovered)
				}
			}()

			return c.Next()
		})
	}

	// 9. Configure CORS
	if err := configureCORS(app, cfg); err != nil {
		slog.Error("CORS configuration failed", "error", err)
		os.Exit(1)
	}

	// 10. Security Headers
	app.Use(helmet.New())

	// 11. Background Worker
	bgWorker := worker.NewBackgroundWorker()

	// 12. Register Routes
	if err := handler.RegisterRoutes(app, db, redisClient, cfg); err != nil {
		slog.Error("Failed to register routes", "error", err)
		os.Exit(1)
	}

	// 13. Catch-all 404
	app.Use(func(c fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Route not found",
			"error":   "NOT_FOUND",
		})
	})

	// 14. Periodic Background Automation
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			ctx, cancel := context.WithTimeout(
				context.Background(),
				60*time.Second,
			)

			// Process due auctions.
			if err := processDueProducts(ctx, db); err != nil {
				slog.Warn(
					"processDueProducts failed",
					"error", err,
				)

				if cfg.SentryDSN != "" {
					sentry.WithScope(func(scope *sentry.Scope) {
						scope.SetTag("type", "process_due_products_failure")
						scope.SetTag("component", "background_job")
						sentry.CaptureException(err)
					})
				}
			}

			// Retry unprocessed payments.
			if n, err := retryUnprocessedPayments(ctx, db, cfg); err != nil {
				slog.Warn(
					"retryUnprocessedPayments failed",
					"error", err,
				)
			} else if n > 0 {
				slog.Info(
					"retryUnprocessedPayments fulfilled",
					"count", n,
				)
			}

			cancel()
		}
	}()

	// 15. Start HTTP Server
	go func() {
		addr := fmt.Sprintf(":%s", cfg.AppPort)

		slog.Info("Starting HTTP server", "address", addr)

		if err := app.Listen(addr); err != nil {
			slog.Error("Server running error", "error", err)
		}
	}()

	// 16. Graceful Shutdown
	quit := make(chan os.Signal, 1)

	signal.Notify(
		quit,
		os.Interrupt,
		syscall.SIGTERM,
	)

	<-quit

	slog.Info("Shutting down admin-backend...")

	if err := app.Shutdown(); err != nil {
		slog.Error("Server shutdown error", "error", err)
	}

	// Wait for background worker tasks.
	bgWorker.Wait()

	// Close Redis.
	if redisClient != nil {
		slog.Info("Redis connection closed")
	}

	// Close Database.
	if db != nil {
		sqlDB, err := db.DB()

		if err == nil {
			slog.Info("Closing database connection...")

			if err := sqlDB.Close(); err != nil {
				slog.Error("Database close error", "error", err)
			}
		}
	}

	slog.Info("Server gracefully stopped")
}

// ============================================================
// CORS CONFIGURATION
// ============================================================

// configureCORS configures a strict origin allowlist.
//
// Allowed origins:
//   - Both configured frontend applications.
//   - Telegram Web.
//   - Telegram Web K.
//   - Telegram Web Z.
//
// Only this middleware manages CORS headers.
func configureCORS(app *fiber.App, cfg *env.Config) error {
	rawOrigins := strings.TrimSpace(cfg.CORSOrigins)

	// Local development defaults.
	if rawOrigins == "" {
		if isProductionEnvironment() {
			return errors.New(
				"CORS_ORIGINS must be explicitly configured in production",
			)
		}

		rawOrigins = "http://localhost:3000,http://localhost:5173"
	}

	allowedOrigins := make(map[string]struct{})

	// Parse configured frontend origins.
	for _, raw := range strings.Split(rawOrigins, ",") {
		origin := strings.TrimSpace(raw)

		if origin == "" {
			continue
		}

		if origin == "*" {
			return errors.New(
				"wildcard CORS origins are not allowed",
			)
		}

		normalized, err := normalizeOrigin(origin)
		if err != nil {
			return fmt.Errorf(
				"invalid CORS origin %q: %w",
				origin,
				err,
			)
		}

		// Production origins must use HTTPS.
		if isProductionEnvironment() &&
			!strings.HasPrefix(normalized, "https://") {
			return fmt.Errorf(
				"production CORS origins must use HTTPS: %s",
				origin,
			)
		}

		allowedOrigins[normalized] = struct{}{}
	}

	// Explicitly trusted Telegram origins.
	// These are retained from the existing implementation.
	telegramOrigins := []string{
		"https://web.telegram.org",
		"https://webk.telegram.org",
		"https://webz.telegram.org",
	}

	for _, origin := range telegramOrigins {
		normalized, err := normalizeOrigin(origin)
		if err != nil {
			return fmt.Errorf(
				"invalid Telegram origin %q: %w",
				origin,
				err,
			)
		}

		allowedOrigins[normalized] = struct{}{}
	}

	if len(allowedOrigins) == 0 {
		return errors.New("no valid CORS origins configured")
	}

	// Log configured origins.
	originList := make([]string, 0, len(allowedOrigins))

	for origin := range allowedOrigins {
		originList = append(originList, origin)
	}

	slog.Info(
		"CORS configured successfully",
		"allowed_origins",
		originList,
		"credentials",
		true,
	)

	// Single CORS middleware.
	app.Use(cors.New(cors.Config{
		AllowOriginsFunc: func(origin string) bool {
			if origin == "" || origin == "null" {
				return false
			}

			normalized, err := normalizeOrigin(origin)
			if err != nil {
				return false
			}

			_, allowed := allowedOrigins[normalized]

			return allowed
		},

		AllowMethods: []string{
			fiber.MethodGet,
			fiber.MethodPost,
			fiber.MethodPut,
			fiber.MethodPatch,
			fiber.MethodDelete,
			fiber.MethodOptions,
		},

		AllowHeaders: []string{
			"Origin",
			"Content-Type",
			"Accept",
			"Authorization",
			"X-CSRF-Token",
			"X-Requested-With",
			"X-Client-Secret",
		},

		AllowCredentials: true,

		MaxAge: 3600,
	}))

	return nil
}

// normalizeOrigin validates and normalizes an HTTP origin.
//
// An origin must contain only:
// scheme + host + optional port.
//
// Examples:
//   - https://example.com
//   - http://localhost:3000
//
// Paths, query strings, fragments, user information,
// wildcard hosts, and unsupported schemes are rejected.
func normalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return "", errors.New("origin cannot be empty")
	}

	if raw == "*" || raw == "null" {
		return "", errors.New("wildcard and null origins are not allowed")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("failed to parse origin: %w", err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("origin must contain a scheme and host")
	}

	if parsed.Opaque != "" {
		return "", errors.New("opaque URLs are not valid origins")
	}

	if parsed.User != nil {
		return "", errors.New("userinfo is not allowed in origins")
	}

	if parsed.Path != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		parsed.ForceQuery ||
		strings.Contains(raw, "#") {
		return "", errors.New(
			"origin must not contain a path, query, or fragment",
		)
	}

	scheme := strings.ToLower(parsed.Scheme)

	if scheme != "http" && scheme != "https" {
		return "", errors.New(
			"only HTTP and HTTPS origins are supported",
		)
	}

	if parsed.Hostname() == "" {
		return "", errors.New("invalid origin hostname")
	}

	if strings.Contains(parsed.Host, "*") {
		return "", errors.New("wildcard hosts are not allowed")
	}

	// Normalize scheme and host for consistent matching.
	return scheme + "://" + strings.ToLower(parsed.Host), nil
}

// ============================================================
// AUCTION AUTOMATION
// ============================================================

func processDueProducts(ctx context.Context, db *gorm.DB) error {
	auctionRepo := postgres.NewAuctionRepository(db)
	partRepo := postgres.NewAuctionParticipationRepository(db)
	bidRepo := postgres.NewBidRepository(db)

	auditRepo := postgres.NewAuditLogRepository(db)
	auditSvc := services.NewAuditLogService(auditRepo)

	notificationRepo := postgres.NewNotificationRepository(db)
	userRepo := postgres.NewUserRepository(db)
	participantUserRepo := postgres.NewParticipantUserRepository(db)

	notifSvc := notifications.NewService(
		notificationRepo,
		userRepo,
		participantUserRepo,
		partRepo,
		nil,
		nil,
		nil,
	)

	lubSvc := lub.NewService(
		auctionRepo,
		partRepo,
		bidRepo,
		auditSvc,
		notifSvc,
		nil,
	)

	return lubSvc.ProcessDueAuctions(ctx)
}

// ============================================================
// CHAPA PAYMENT RETRIES
// ============================================================

func retryUnprocessedPayments(
	ctx context.Context,
	db *gorm.DB,
	cfg *env.Config,
) (int, error) {
	if cfg == nil ||
		strings.TrimSpace(cfg.ChapaSecretKey) == "" {
		return 0, nil
	}

	auctionRepo := postgres.NewAuctionRepository(db)
	partRepo := postgres.NewAuctionParticipationRepository(db)
	bidRepo := postgres.NewBidRepository(db)

	auditRepo := postgres.NewAuditLogRepository(db)
	auditSvc := services.NewAuditLogService(auditRepo)

	notificationRepo := postgres.NewNotificationRepository(db)
	userRepo := postgres.NewUserRepository(db)
	participantUserRepo := postgres.NewParticipantUserRepository(db)

	notifSvc := notifications.NewService(
		notificationRepo,
		userRepo,
		participantUserRepo,
		partRepo,
		nil,
		nil,
		nil,
	)

	tgPoster := telegramadapter.NewChannelPoster(
		cfg,
		slog.Default(),
	)

	notifSvc.SetTelegramMessenger(tgPoster)

	lubSvc := lub.NewService(
		auctionRepo,
		partRepo,
		bidRepo,
		auditSvc,
		notifSvc,
		tgPoster,
	)

	paymentLedgerRepo := postgres.NewPaymentLedgerRepository(db)

	chapaSvc := chapapayment.NewService(
		paymentLedgerRepo,
		lubSvc,
		participantUserRepo,
		notifSvc,
		slog.Default(),
	)

	baseURL := cfg.ChapaBaseURL

	if strings.TrimSpace(cfg.ChapaAPIURL) != "" {
		baseURL = strings.TrimSpace(cfg.ChapaAPIURL)
	}

	chapaSvc.SetChapaClient(
		payment.NewChapaClient(
			cfg.ChapaSecretKey,
			baseURL,
		),
	)

	chapaSvc.SetAdminNotifier(tgPoster)

	fulfilled, err := chapaSvc.RetryUnprocessedPayments(ctx, 50)
	if err != nil {
		return fulfilled, err
	}

	reconciled, err := chapaSvc.ReconcilePendingPayments(
		ctx,
		2*time.Minute,
		50,
	)

	return fulfilled + reconciled, err
}

// ============================================================
// ENVIRONMENT
// ============================================================

func isProductionEnvironment() bool {
	environment := strings.TrimSpace(os.Getenv("ENVIRONMENT"))

	return strings.EqualFold(environment, "production") ||
		strings.EqualFold(environment, "prod")
}
