-- +goose Up
-- Agent and influencer referrals: codes, link clicks, attributed signups (qualified once they buy
-- a paid ticket), payout programs and recorded payouts. Mirrors platform/database/referrals.go,
-- which is what actually runs (EnsureSafeSchema); keep the two in step.

CREATE TABLE IF NOT EXISTS referral_programs (
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
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_programs_default ON referral_programs (kind) WHERE is_default;

CREATE TABLE IF NOT EXISTS referral_codes (
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
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_codes_code ON referral_codes (code);
CREATE INDEX IF NOT EXISTS idx_referral_codes_owner ON referral_codes (owner_user_id);

CREATE TABLE IF NOT EXISTS referral_clicks (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  code_id       UUID NOT NULL REFERENCES referral_codes(id) ON UPDATE CASCADE ON DELETE CASCADE,
  visitor_id    VARCHAR(64) NOT NULL,
  hits          INTEGER NOT NULL DEFAULT 1,
  ip_hash       VARCHAR(64) NOT NULL DEFAULT '',
  user_agent    VARCHAR(255) NOT NULL DEFAULT '',
  landing_path  VARCHAR(255) NOT NULL DEFAULT '',
  first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_clicks_visitor ON referral_clicks (code_id, visitor_id);
CREATE INDEX IF NOT EXISTS idx_referral_clicks_code_time ON referral_clicks (code_id, first_seen_at);

CREATE TABLE IF NOT EXISTS referral_attributions (
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
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_attr_user ON referral_attributions (referred_user_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_attr_phone ON referral_attributions (qualified_phone) WHERE status = 'qualified';
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_attr_account ON referral_attributions (qualified_account) WHERE status = 'qualified' AND qualified_account IS NOT NULL AND qualified_account <> '';
CREATE INDEX IF NOT EXISTS idx_referral_attr_code_status ON referral_attributions (code_id, status);
CREATE INDEX IF NOT EXISTS idx_referral_attr_pending ON referral_attributions (signed_up_at) WHERE status = 'signed_up';

CREATE TABLE IF NOT EXISTS referral_payouts (
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
);
CREATE INDEX IF NOT EXISTS idx_referral_payouts_code ON referral_payouts (code_id, paid_at);

-- Default programs (threshold 10; 11-25: 5 ETB, 26-50: 10 ETB, 51+: 20 ETB; margin 25 ETB).
INSERT INTO referral_programs (name, kind, is_default, threshold, tiers, net_margin_per_bid)
VALUES
  ('Agent program', 'agent', true, 10, '[{"from":11,"to":25,"amount":5},{"from":26,"to":50,"amount":10},{"from":51,"amount":20}]', 25),
  ('Influencer program', 'influencer', true, 10, '[{"from":11,"to":25,"amount":5},{"from":26,"to":50,"amount":10},{"from":51,"amount":20}]', 25)
ON CONFLICT (kind) WHERE is_default DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS referral_payouts;
DROP TABLE IF EXISTS referral_attributions;
DROP TABLE IF EXISTS referral_clicks;
DROP TABLE IF EXISTS referral_codes;
DROP TABLE IF EXISTS referral_programs;
