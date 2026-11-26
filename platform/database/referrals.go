package database

import (
	"encoding/json"
	"log/slog"

	"github.com/bid-hub/admin-backend/core/domain"
	"gorm.io/gorm"
)

// referralSchemaSQL creates the agent/influencer referral tables (see domain/referral.go and
// migrations/00018_referrals.sql). Every statement is idempotent; production only runs
// EnsureSafeSchema, so the tables are created here rather than by AutoMigrate.
var referralSchemaSQL = []string{
	`CREATE TABLE IF NOT EXISTS referral_programs (
		id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name                    VARCHAR(120) NOT NULL,
		kind                    VARCHAR(20) NOT NULL,
		is_default              BOOLEAN NOT NULL DEFAULT false,
		threshold               INTEGER NOT NULL,
		tiers                   JSONB NOT NULL,
		net_margin_per_bid      DECIMAL(12,2) NOT NULL,
		qualifying_auction_ids  JSONB NOT NULL DEFAULT '[]'::jsonb,
		attribution_window_days INTEGER NOT NULL DEFAULT 30,
		created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT chk_referral_programs_kind CHECK (kind IN ('agent', 'influencer')),
		CONSTRAINT chk_referral_programs_threshold CHECK (threshold >= 0),
		CONSTRAINT chk_referral_programs_margin CHECK (net_margin_per_bid > 0),
		CONSTRAINT chk_referral_programs_window CHECK (attribution_window_days BETWEEN 1 AND 365)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_programs_default ON referral_programs (kind) WHERE is_default`,

	`CREATE TABLE IF NOT EXISTS referral_codes (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		code          VARCHAR(20) NOT NULL,
		kind          VARCHAR(20) NOT NULL,
		owner_user_id UUID REFERENCES users(id) ON UPDATE CASCADE ON DELETE SET NULL,
		label         VARCHAR(120) NOT NULL DEFAULT '',
		program_id    UUID REFERENCES referral_programs(id) ON UPDATE CASCADE ON DELETE SET NULL,
		status        VARCHAR(20) NOT NULL DEFAULT 'active',
		created_by    UUID,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT chk_referral_codes_kind CHECK (kind IN ('agent', 'influencer')),
		CONSTRAINT chk_referral_codes_status CHECK (status IN ('active', 'paused')),
		CONSTRAINT chk_referral_codes_upper CHECK (code = upper(code))
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_codes_code ON referral_codes (code)`,
	`CREATE INDEX IF NOT EXISTS idx_referral_codes_owner ON referral_codes (owner_user_id)`,

	`CREATE TABLE IF NOT EXISTS referral_clicks (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		code_id       UUID NOT NULL REFERENCES referral_codes(id) ON UPDATE CASCADE ON DELETE CASCADE,
		visitor_id    VARCHAR(64) NOT NULL,
		hits          INTEGER NOT NULL DEFAULT 1,
		ip_hash       VARCHAR(64) NOT NULL DEFAULT '',
		user_agent    VARCHAR(255) NOT NULL DEFAULT '',
		landing_path  VARCHAR(255) NOT NULL DEFAULT '',
		first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_clicks_visitor ON referral_clicks (code_id, visitor_id)`,
	`CREATE INDEX IF NOT EXISTS idx_referral_clicks_code_time ON referral_clicks (code_id, first_seen_at)`,

	`CREATE TABLE IF NOT EXISTS referral_attributions (
		id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		code_id               UUID NOT NULL REFERENCES referral_codes(id) ON UPDATE CASCADE ON DELETE CASCADE,
		referred_user_id      UUID NOT NULL REFERENCES users(id) ON UPDATE CASCADE ON DELETE CASCADE,
		visitor_id            VARCHAR(64) NOT NULL DEFAULT '',
		status                VARCHAR(20) NOT NULL DEFAULT 'signed_up',
		signed_up_at          TIMESTAMPTZ NOT NULL,
		qualified_at          TIMESTAMPTZ,
		qualifying_tx_ref     VARCHAR(255) NOT NULL DEFAULT '',
		qualifying_auction_id UUID,
		qualified_phone       VARCHAR(32),
		qualified_account     VARCHAR(255),
		reject_reason         VARCHAR(40) NOT NULL DEFAULT '',
		reject_note           VARCHAR(255) NOT NULL DEFAULT '',
		created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT chk_referral_attr_status CHECK (status IN ('signed_up', 'qualified', 'rejected')),
		CONSTRAINT chk_referral_attr_qualified CHECK (
			status <> 'qualified' OR (qualified_at IS NOT NULL AND qualified_phone IS NOT NULL AND qualified_phone <> '')
		)
	)`,
	// One attribution per account (first code wins).
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_attr_user ON referral_attributions (referred_user_id)`,
	// Anti-fraud backstop: a phone number and a payment account each qualify one referral, ever.
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_attr_phone ON referral_attributions (qualified_phone) WHERE status = 'qualified'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_attr_account ON referral_attributions (qualified_account) WHERE status = 'qualified' AND qualified_account IS NOT NULL AND qualified_account <> ''`,
	`CREATE INDEX IF NOT EXISTS idx_referral_attr_code_status ON referral_attributions (code_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_referral_attr_pending ON referral_attributions (signed_up_at) WHERE status = 'signed_up'`,

	`CREATE TABLE IF NOT EXISTS referral_payouts (
		id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		code_id    UUID NOT NULL REFERENCES referral_codes(id) ON UPDATE CASCADE ON DELETE CASCADE,
		amount     DECIMAL(12,2) NOT NULL,
		method     VARCHAR(40) NOT NULL DEFAULT '',
		reference  VARCHAR(120) NOT NULL DEFAULT '',
		note       VARCHAR(255) NOT NULL DEFAULT '',
		paid_by    UUID,
		paid_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT chk_referral_payouts_amount CHECK (amount > 0)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_referral_payouts_code ON referral_payouts (code_id, paid_at)`,
}

// ensureReferralSchema creates the referral tables and the default program for each kind.
func ensureReferralSchema(db *gorm.DB) {
	// On a fresh database the first EnsureSafeSchema pass runs before AutoMigrate creates users;
	// Migrate calls EnsureSafeSchema again afterwards.
	var usersTable *string
	if err := db.Raw(`SELECT to_regclass('public.users')::text`).Scan(&usersTable).Error; err != nil || usersTable == nil {
		return
	}
	for _, stmt := range referralSchemaSQL {
		if err := db.Exec(stmt).Error; err != nil {
			slog.Error("database: referral schema patch failed", "error", err)
			return
		}
	}
	for _, kind := range []string{domain.ReferralKindAgent, domain.ReferralKindInfluencer} {
		p := domain.NewDefaultReferralProgram(kind)
		tiers, err := json.Marshal(p.Tiers)
		if err != nil {
			slog.Error("database: referral default tiers", "error", err)
			return
		}
		// ON CONFLICT on the partial unique index keeps this a no-op once a default exists, so
		// staff edits to the default program are never overwritten.
		if err := db.Exec(`INSERT INTO referral_programs
			(name, kind, is_default, threshold, tiers, net_margin_per_bid, qualifying_auction_ids, attribution_window_days)
			VALUES (?, ?, true, ?, ?::jsonb, ?, '[]'::jsonb, ?)
			ON CONFLICT (kind) WHERE is_default DO NOTHING`,
			p.Name, p.Kind, p.Threshold, string(tiers), p.NetMarginPerBid, p.AttributionWindowDays,
		).Error; err != nil {
			slog.Error("database: referral default program", "kind", kind, "error", err)
		}
	}
}
