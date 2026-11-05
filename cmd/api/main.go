package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// 1b. Initialize Sentry for error tracking and structured logging.
	// Tuned for the Sentry Free plan (5,000 errors/month)   only sends
	// actionable errors so quota is not wasted on noise.
	if cfg.SentryDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			Environment:      cfg.SentryEnvironment,
			TracesSampleRate: cfg.SentryTracesSampleRate,
			EnableLogs:       true,

			// Drop routine log levels so they don't count against the error quota.
			BeforeSendLog: func(log *sentry.Log) *sentry.Log {
				if log.Severity <= sentry.LogSeverityInfo {
					return nil // only warn/error/fatal reach Sentry
				}
				return log
			},

			BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
				// Drop health-check noise   never a real error
				if event.Request != nil {
					url := event.Request.URL
					if url == "/health" || url == "/api/health" {
						return nil
					}
				}
				// Strip sensitive auth headers before the event leaves the process
				if event.Request != nil {
					for k := range event.Request.Headers {
						if k == "Authorization" || k == "Cookie" || k == "X-Csrf-Token" {
							delete(event.Request.Headers, k)
						}
					}
				}
				return event
			},
		}); err != nil {
			slog.Warn("Sentry initialization failed   continuing without error tracking", "error", err)
		} else {
			slog.Info("Sentry error tracking initialized", "environment", cfg.SentryEnvironment)
			defer sentry.Flush(3 * time.Second)
		}
	} else {
		slog.Warn("SENTRY_DSN not set   error tracking disabled")
	}

	if err := cfg.Validate(); err != nil {
		slog.Error("Configuration validation failed", "error", err)
		os.Exit(1)
	}

	// 2. Connect to Database
	db, err := database.Connect(cfg)
	if err != nil {
		slog.Error("Critical: Could not connect to database", "error", err)
		os.Exit(1)
	}

	// Always apply safe additive patches (e.g. new columns). Full AutoMigrate stays
	// non-production only so prod schema changes stay explicit/controlled.
	database.EnsureSafeSchema(db)

	if !isProductionEnvironment(cfg) {
		slog.Info("Development environment - running database migrations")
		database.Migrate(db)

	} else {
		slog.Info("Production environment - applied safe schema patches; skipping full AutoMigrate/seed")
	}

	// 2b. Connect to Redis
	redisClient := cache.InitRedis(cfg)
	if redisClient != nil {
		defer redisClient.Close()
	}

	// 3. Initialize Fiber App
	app := fiber.New(fiber.Config{
		BodyLimit: 100 * 1024 * 1024, // 100MB buffer for multi-file/video media uploads
		ErrorHandler: func(c fiber.Ctx, err error) error {
			// validate.Fail / BindAndValidate already wrote the response envelope.
			if validate.IsValidation(err) {
				return nil
			}
			// Map domain error to HTTP code and standard string code
			code, errCode := domain.MapDomainErrorToHTTP(err)
			msg := err.Error()

			// If it's a fiber.Error, use its internal metadata
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
				errCode = domain.MapHTTPToErrorCode(code)
				msg = fiberErr.Message
			}

			// Ensure CORS headers are injected even when Fiber rejects early (e.g. 413 Request Entity Too Large)
			if origin := c.Get("Origin"); origin != "" {
				c.Set("Vary", "Origin")
				c.Set("Access-Control-Allow-Origin", origin)
				c.Set("Access-Control-Allow-Credentials", "true")
			}

			// Return standardized JSON response
			// Capture unexpected 5xx server errors to Sentry (only if initialized)
			if code >= 500 && cfg.SentryDSN != "" {
				sentry.CaptureException(err)
			}
			return response.Error(c, code, msg, errCode)
		},
	})

	// IMPORTANT: Recover must be the very first middleware so panics from ANY
	// handler (including the CSRF group) are always caught safely.
	app.Use(fiberRecover.New(fiberRecover.Config{
		EnableStackTrace: false,
		StackTraceHandler: func(c fiber.Ctx, e any) {
			// no-op: keep stack traces out of production logs
		},
	}))
	app.Use(handler.RequestID())
	app.Use(logger.New(logger.Config{
		Format: `{"time":"${time}","status":${status},"method":"${method}","path":"${path}","latency":"${latency}","ip":"${ip}","user_agent":"${ua}","error":"${error}"}` + "\n",
	}))

	// Custom Sentry Middleware for Fiber v3 (sentryfiber only supports Fiber v2)
	if cfg.SentryDSN != "" {
		app.Use(func(c fiber.Ctx) error {
			hub := sentry.CurrentHub().Clone()
			hub.Scope().SetTag("transaction", c.Method()+" "+c.Path())
			hub.Scope().SetContext("Request", map[string]interface{}{
				"method": c.Method(),
				"path":   c.Path(),
				"ip":     c.IP(),
			})
			defer func() {
				if recovered := recover(); recovered != nil {
					eventID := hub.RecoverWithContext(c.Context(), recovered)
					if eventID != nil {
						sentry.Flush(2 * time.Second)
					}
					panic(recovered) // re-panic for Fiber's recover middleware
				}
			}()
			return c.Next()
		})
	}
	corsOrigins := cfg.CORSOrigins
	if corsOrigins == "" {
		corsOrigins = "http://localhost:3000,http://localhost:5173"
	}

	rawOrigins := strings.Split(corsOrigins, ",")
	origins := make([]string, 0, len(rawOrigins)+3)
	originSeen := make(map[string]struct{}, len(rawOrigins)+3)
	for _, raw := range rawOrigins {
		o := strings.TrimSpace(raw)
		if o == "" || o == "*" {
			if o == "*" {
				slog.Warn("admin-backend: ignoring CORS_ORIGINS='*' (incompatible with credentialed CORS)")
			}
			continue
		}
		if _, ok := originSeen[o]; !ok {
			originSeen[o] = struct{}{}
			origins = append(origins, o)
		}
	}
	for _, tgOrigin := range []string{
		"https://web.telegram.org",
		"https://webk.telegram.org",
		"https://webz.telegram.org",
	} {
		if _, ok := originSeen[tgOrigin]; !ok {
			originSeen[tgOrigin] = struct{}{}
			origins = append(origins, tgOrigin)
		}
	}
	if len(origins) == 0 {
		slog.Error("admin-backend: no valid CORS origins configured. Set CORS_ORIGINS to a comma-separated list of explicit origins (wildcards are not permitted with credentialed CORS).")
		os.Exit(1)
	}

	isTelegramOrigin := func(origin string) bool {
		if !strings.HasPrefix(origin, "https://") {
			return false
		}
		host := strings.TrimPrefix(origin, "https://")
		return host == "telegram.org" || strings.HasSuffix(host, ".telegram.org")
	}

	// Ensure CORS headers are present even on error responses (e.g. 5xx),
	// so the browser can read the JSON error payload.
	allowedOrigin := func(origin string) (string, bool) {
		o := strings.TrimSpace(origin)
		if o == "" {
			return "", false
		}
		for _, allowed := range origins {
			if allowed == o {
				return o, true
			}
		}
		if isTelegramOrigin(o) {
			return o, true
		}
		return "", false
	}

	app.Use(func(c fiber.Ctx) error {
		if origin, ok := allowedOrigin(c.Get("Origin")); ok {
			c.Set("Vary", "Origin")
			c.Set("Access-Control-Allow-Origin", origin)
			c.Set("Access-Control-Allow-Credentials", "true")
			c.Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,PATCH,OPTIONS")
			c.Set("Access-Control-Allow-Headers", "Origin,Content-Type,Accept,Authorization,X-CSRF-Token,X-Requested-With,X-Client-Secret")
		}

		if c.Method() == fiber.MethodOptions {
			return c.SendStatus(fiber.StatusNoContent)
		}

		return c.Next()
	})

	app.Use(cors.New(cors.Config{
		AllowOriginsFunc: func(origin string) bool {
			_, ok := allowedOrigin(origin)
			return ok
		},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-CSRF-Token", "X-Requested-With", "X-Client-Secret"},
		AllowCredentials: true,
		MaxAge:           3600,
	}))
	app.Use(helmet.New())

	// 5. Initialize Background Worker and Routes
	bgWorker := worker.NewBackgroundWorker()
	err = handler.RegisterRoutes(app, db, redisClient, cfg)
	if err != nil {
		slog.Error("Failed to register routes (e.g. email templates)", "error", err)
		os.Exit(1)
	}

	// Catch-all: return a clean 404 JSON instead of Fiber's default 501.
	app.Use(func(c fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Route not found",
			"error":   "NOT_FOUND",
		})
	})

	// Periodic automation: auto-close due auctions + fulfill stuck Chapa payments.
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := processDueProducts(ctx, db); err != nil {
				slog.Warn("processDueProducts failed", "error", err)
				if cfg.SentryDSN != "" {
					sentry.WithScope(func(scope *sentry.Scope) {
						scope.SetTag("type", "process_due_products_failure")
						scope.SetTag("component", "background_job")
						sentry.CaptureException(err)
					})
				}
			}
			if n, err := retryUnprocessedPayments(ctx, db, cfg); err != nil {
				slog.Warn("retryUnprocessedPayments failed", "error", err)
			} else if n > 0 {
				slog.Info("retryUnprocessedPayments fulfilled", "count", n)
			}
			cancel()
		}
	}()

	// 6. Start Server in a goroutine
	go func() {
		if err := app.Listen(fmt.Sprintf(":%s", cfg.AppPort)); err != nil {
			slog.Error("Server running error", "error", err)
		}
	}()

	// 7. Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	<-quit
	slog.Info("Shutting down admin-backend...")

	// Shutdown Fiber
	if err := app.Shutdown(); err != nil {
		slog.Error("Server Shutdown Error", "error", err)
	}

	// Wait for background tasks to finish
	bgWorker.Wait()

	// Close DB connection
	if db != nil {
		sqlDB, err := db.DB()
		if err == nil {
			slog.Info("Closing database connection...")
			sqlDB.Close()
		}
	}

	slog.Info("Server gracefully stopped")
}

func processDueProducts(ctx context.Context, db *gorm.DB) error {
	auctionRepo := postgres.NewAuctionRepository(db)
	partRepo := postgres.NewAuctionParticipationRepository(db)
	bidRepo := postgres.NewBidRepository(db)
	auditRepo := postgres.NewAuditLogRepository(db)
	auditSvc := services.NewAuditLogService(auditRepo)
	notificationRepo := postgres.NewNotificationRepository(db)
	userRepo := postgres.NewUserRepository(db)
	participantUserRepo := postgres.NewParticipantUserRepository(db)
	notifSvc := notifications.NewService(notificationRepo, userRepo, participantUserRepo, partRepo, nil, nil, nil)
	lubSvc := lub.NewService(auctionRepo, partRepo, bidRepo, auditSvc, notifSvc, nil)
	return lubSvc.ProcessDueAuctions(ctx)
}

func retryUnprocessedPayments(ctx context.Context, db *gorm.DB, cfg *env.Config) (int, error) {
	if cfg == nil || strings.TrimSpace(cfg.ChapaSecretKey) == "" {
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
	notifSvc := notifications.NewService(notificationRepo, userRepo, participantUserRepo, partRepo, nil, nil, nil)
	tgPoster := telegramadapter.NewChannelPoster(cfg, slog.Default())
	notifSvc.SetTelegramMessenger(tgPoster)
	lubSvc := lub.NewService(auctionRepo, partRepo, bidRepo, auditSvc, notifSvc, tgPoster)
	paymentLedgerRepo := postgres.NewPaymentLedgerRepository(db)
	chapaSvc := chapapayment.NewService(paymentLedgerRepo, lubSvc, participantUserRepo, notifSvc, slog.Default())

	baseURL := cfg.ChapaBaseURL
	if strings.TrimSpace(cfg.ChapaAPIURL) != "" {
		baseURL = strings.TrimSpace(cfg.ChapaAPIURL)
	}
	chapaSvc.SetChapaClient(payment.NewChapaClient(cfg.ChapaSecretKey, baseURL))
	chapaSvc.SetAdminNotifier(tgPoster)

	fulfilled, err := chapaSvc.RetryUnprocessedPayments(ctx, 50)
	if err != nil {
		return fulfilled, err
	}
	reconciled, err := chapaSvc.ReconcilePendingPayments(ctx, 2*time.Minute, 50)
	return fulfilled + reconciled, err
}

// isProductionEnvironment checks if the environment is production
func isProductionEnvironment(cfg *env.Config) bool {
	env := os.Getenv("ENVIRONMENT")
	if env == "production" || env == "prod" {
		return true
	}
	return false
}
