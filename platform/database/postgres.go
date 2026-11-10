package database

import (
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/platform/env"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// waivedPerUserIndexSQL enforces "one free entrance per customer": a customer may hold at most
// one pending/successful free-entrance checkout. It is skipped while payments.status is still the
// legacy varchar (the enum conversion would otherwise trip over the index predicate); Migrate
// re-applies it after the conversion.
const waivedPerUserIndexSQL = `DO $$ BEGIN
	IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'payments' AND column_name = 'status' AND udt_name = 'payment_status')
	   AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'payments' AND column_name = 'entrance_waived') THEN
		CREATE UNIQUE INDEX IF NOT EXISTS ux_payments_one_waived_per_user
			ON payments (user_id)
			WHERE entrance_waived = true AND status IN ('pending'::payment_status, 'success'::payment_status);
	END IF;
END $$;`

// EnsureSafeSchema applies additive, idempotent column patches that production
// needs even when full AutoMigrate is skipped.
func EnsureSafeSchema(db *gorm.DB) {
	if db == nil {
		return
	}
	patches := []string{
		`ALTER TABLE auctions ADD COLUMN IF NOT EXISTS image_urls JSONB NOT NULL DEFAULT '[]'::jsonb`,
		`ALTER TABLE auctions ADD COLUMN IF NOT EXISTS retail_value DECIMAL(12,2) NOT NULL DEFAULT 0`,
		`ALTER TABLE auctions ADD COLUMN IF NOT EXISTS min_participants INTEGER NOT NULL DEFAULT 10`,
		// Free-entrance promo is opt-in per auction: existing and new rows default to OFF.
		`ALTER TABLE auctions ADD COLUMN IF NOT EXISTS free_entrance_enabled BOOLEAN NOT NULL DEFAULT false`,
		`ALTER TABLE auctions ALTER COLUMN free_entrance_enabled SET DEFAULT false`,
		`ALTER TABLE auctions ADD COLUMN IF NOT EXISTS free_entrance_limit INTEGER NOT NULL DEFAULT 100`,
		`ALTER TABLE auctions ADD COLUMN IF NOT EXISTS free_entrance_claimed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE auction_participations ADD COLUMN IF NOT EXISTS entrance_free BOOLEAN NOT NULL DEFAULT false`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_participation_one_free_per_user ON auction_participations (participant_id) WHERE entrance_free = true AND status = 'active'`,
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS bid_values TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS entrance_waived BOOLEAN NOT NULL DEFAULT false`,
		waivedPerUserIndexSQL,
		`UPDATE auctions
		 SET image_urls = jsonb_build_array(image_url)
		 WHERE (image_urls IS NULL OR image_urls = '[]'::jsonb)
		   AND image_url IS NOT NULL
		   AND btrim(image_url) <> ''`,
	}
	for _, stmt := range patches {
		if err := db.Exec(stmt).Error; err != nil {
			slog.Error("database: safe schema patch failed", "error", err)
		}
	}
}

func Migrate(db *gorm.DB) {
	if db == nil {
		return
	}

	EnsureSafeSchema(db)

	enumMigrations := []string{
		// 1. Create PostgreSQL ENUM types idempotently
		`DO $$ BEGIN
			CREATE TYPE auction_status AS ENUM ('draft', 'active', 'postponed', 'closed', 'executed', 'cancelled');
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			CREATE TYPE user_status AS ENUM ('active', 'suspended', 'pending');
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			CREATE TYPE payment_status AS ENUM ('pending', 'success', 'failed', 'refunded');
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			CREATE TYPE refund_status AS ENUM ('pending', 'processing', 'success', 'failed', 'reversed');
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			CREATE TYPE participant_user_status AS ENUM ('active', 'suspended');
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			CREATE TYPE participation_status AS ENUM ('active', 'refunded');
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// 2. Convert existing varchar status columns to their ENUM types (if tables already exist)
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'auctions' AND column_name = 'status' AND udt_name <> 'auction_status') THEN
				ALTER TABLE auctions DROP CONSTRAINT IF EXISTS chk_auctions_status;
				ALTER TABLE auctions ALTER COLUMN status DROP DEFAULT;
				ALTER TABLE auctions ALTER COLUMN status TYPE auction_status USING status::text::auction_status;
				ALTER TABLE auctions ALTER COLUMN status SET DEFAULT 'draft'::auction_status;
			END IF;
		END $$;`,
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'status' AND udt_name <> 'user_status') THEN
				ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_users_status;
				ALTER TABLE users ALTER COLUMN status DROP DEFAULT;
				ALTER TABLE users ALTER COLUMN status TYPE user_status USING status::text::user_status;
				ALTER TABLE users ALTER COLUMN status SET DEFAULT 'active'::user_status;
			END IF;
		END $$;`,
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'payments' AND column_name = 'status' AND udt_name <> 'payment_status') THEN
				ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_status;
				ALTER TABLE payments ALTER COLUMN status DROP DEFAULT;
				ALTER TABLE payments ALTER COLUMN status TYPE payment_status USING status::text::payment_status;
				ALTER TABLE payments ALTER COLUMN status SET DEFAULT 'pending'::payment_status;
			END IF;
		END $$;`,
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'refund_records' AND column_name = 'status' AND udt_name <> 'refund_status') THEN
				ALTER TABLE refund_records DROP CONSTRAINT IF EXISTS chk_refunds_status;
				ALTER TABLE refund_records ALTER COLUMN status DROP DEFAULT;
				ALTER TABLE refund_records ALTER COLUMN status TYPE refund_status USING status::text::refund_status;
				ALTER TABLE refund_records ALTER COLUMN status SET DEFAULT 'pending'::refund_status;
			END IF;
		END $$;`,
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'participant_users' AND column_name = 'status' AND udt_name <> 'participant_user_status') THEN
				ALTER TABLE participant_users DROP CONSTRAINT IF EXISTS chk_participant_users_status;
				ALTER TABLE participant_users ALTER COLUMN status DROP DEFAULT;
				ALTER TABLE participant_users ALTER COLUMN status TYPE participant_user_status USING status::text::participant_user_status;
				ALTER TABLE participant_users ALTER COLUMN status SET DEFAULT 'active'::participant_user_status;
			END IF;
		END $$;`,
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'auction_participations' AND column_name = 'status' AND udt_name <> 'participation_status') THEN
				ALTER TABLE auction_participations DROP CONSTRAINT IF EXISTS chk_participations_status;
				ALTER TABLE auction_participations ALTER COLUMN status DROP DEFAULT;
				ALTER TABLE auction_participations ALTER COLUMN status TYPE participation_status USING status::text::participation_status;
				ALTER TABLE auction_participations ALTER COLUMN status SET DEFAULT 'active'::participation_status;
			END IF;
		END $$;`,
	}

	for _, stmt := range enumMigrations {
		if err := db.Exec(stmt).Error; err != nil {
			slog.Warn("database: enum migration warning", "error", err)
		}
	}

	if err := db.AutoMigrate(
		&domain.User{},
		&domain.ParticipantUser{},
		&domain.Auction{},
		&domain.AuctionParticipation{},
		&domain.Bid{},
		&domain.Payment{},
		&domain.RefundRecord{},
		&domain.Notification{},
		&domain.AuditLog{},
		&domain.PlatformLegalDocument{},
	); err != nil {
		slog.Error("database: automigrate failed", "error", err)
	}

	constraints := []string{
		// Auctions constraints
		`DO $$ BEGIN
			ALTER TABLE auctions ADD CONSTRAINT chk_auctions_entrance_fee CHECK (entrance_fee > 0);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE auctions ADD CONSTRAINT chk_auctions_min_bid_value CHECK (min_bid_value > 0);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE auctions ADD CONSTRAINT chk_auctions_max_bid_value CHECK (max_bid_value IS NULL OR max_bid_value >= min_bid_value);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// Free-entrance promo bounds: limit is 0 ("all") or 1..1,000,000 and the claimed counter
		// may never exceed a finite limit or go negative.
		`DO $$ BEGIN
			ALTER TABLE auctions ADD CONSTRAINT chk_auctions_free_entrance CHECK (
				free_entrance_limit >= 0 AND free_entrance_limit <= 1000000
				AND free_entrance_claimed >= 0
				AND (free_entrance_limit = 0 OR free_entrance_claimed <= free_entrance_limit)
			);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		waivedPerUserIndexSQL,

		// Auction participations constraints (fee_paid is always > 0: free entrance still pays the bid)
		`DO $$ BEGIN
			ALTER TABLE auction_participations ADD CONSTRAINT chk_participations_fee_paid CHECK (fee_paid > 0);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE auction_participations ADD CONSTRAINT fk_participations_auction FOREIGN KEY (auction_id) REFERENCES auctions(id) ON UPDATE CASCADE ON DELETE CASCADE;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// Auction bids constraints
		`DO $$ BEGIN
			ALTER TABLE auction_bids ADD CONSTRAINT chk_bids_bid_value CHECK (bid_value > 0);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE auction_bids ADD CONSTRAINT fk_bids_auction FOREIGN KEY (auction_id) REFERENCES auctions(id) ON UPDATE CASCADE ON DELETE CASCADE;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// Winner bid FK on auctions (deferred after auction_bids exists)
		`DO $$ BEGIN
			ALTER TABLE auctions ADD CONSTRAINT fk_auctions_winner_bid FOREIGN KEY (winner_bid_id) REFERENCES auction_bids(id) ON UPDATE CASCADE ON DELETE SET NULL;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// Payments constraints
		`DO $$ BEGIN
			ALTER TABLE payments ADD CONSTRAINT chk_payments_amount CHECK (amount >= 0);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE payments ADD CONSTRAINT fk_payments_auction FOREIGN KEY (auction_id) REFERENCES auctions(id) ON UPDATE CASCADE ON DELETE SET NULL;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE payments ADD CONSTRAINT fk_payments_participation FOREIGN KEY (participation_id) REFERENCES auction_participations(id) ON UPDATE CASCADE ON DELETE SET NULL;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// Refund records constraints
		`DO $$ BEGIN
			ALTER TABLE refund_records ADD CONSTRAINT chk_refunds_amount CHECK (amount > 0);
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE refund_records ADD CONSTRAINT fk_refunds_auction FOREIGN KEY (auction_id) REFERENCES auctions(id) ON UPDATE CASCADE ON DELETE RESTRICT;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
		`DO $$ BEGIN
			ALTER TABLE refund_records ADD CONSTRAINT fk_refunds_participation FOREIGN KEY (participation_id) REFERENCES auction_participations(id) ON UPDATE CASCADE ON DELETE SET NULL;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,

		// Notifications FK
		`DO $$ BEGIN
			ALTER TABLE notifications ADD CONSTRAINT fk_notifications_auction FOREIGN KEY (auction_id) REFERENCES auctions(id) ON UPDATE CASCADE ON DELETE SET NULL;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$;`,
	}

	for _, stmt := range constraints {
		if err := db.Exec(stmt).Error; err != nil {
			slog.Warn("database: constraint setup warning", "error", err)
		}
	}
}

func Connect(cfg *env.Config) (*gorm.DB, error) {
	dsn, err := databaseDSN(cfg)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxIdleConns(2)
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
		sqlDB.SetConnMaxIdleTime(10 * time.Minute)
	}

	slog.Info("connected to PostgreSQL", "max_idle_conns", 2, "max_open_conns", 10, "max_conn_max_lifetime", "30m", "max_conn_max_idle_time", "10m")
	return db, nil
}

func databaseDSN(cfg *env.Config) (string, error) {
	if cfg.DatabaseURL == "" {
		return "", fmt.Errorf("DB_URL is required")
	}

	parsed, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		return "", fmt.Errorf("DB_URL is not a valid URL: %w", err)
	}

	query := parsed.Query()
	if query.Get("sslmode") == "" && cfg.DBSSLMode != "" {
		query.Set("sslmode", cfg.DBSSLMode)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
