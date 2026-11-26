-- +goose Up
-- Self-serve agents: a participant becomes an agent from the app and gets a code straight away.
-- Applied at startup by database.EnsureSafeSchema (platform/database/referrals.go); this file
-- mirrors it.

-- Where each code owner wants to be paid. Empty until they add it on the agent dashboard.
ALTER TABLE referral_codes ADD COLUMN IF NOT EXISTS payout_method VARCHAR(20) NOT NULL DEFAULT '';
ALTER TABLE referral_codes ADD COLUMN IF NOT EXISTS payout_bank VARCHAR(60) NOT NULL DEFAULT '';
ALTER TABLE referral_codes ADD COLUMN IF NOT EXISTS payout_account VARCHAR(40) NOT NULL DEFAULT '';
ALTER TABLE referral_codes ADD COLUMN IF NOT EXISTS payout_name VARCHAR(120) NOT NULL DEFAULT '';
-- Two agents can't be paid into the same wallet or bank account.
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_codes_payout ON referral_codes (payout_method, payout_bank, payout_account) WHERE payout_account <> '';
-- One agent code per account. Skipped at startup (with a warning) if staff already made two.
CREATE UNIQUE INDEX IF NOT EXISTS ux_referral_codes_agent_owner ON referral_codes (owner_user_id) WHERE kind = 'agent' AND owner_user_id IS NOT NULL;

-- Staff switch for self-serve agent signup (one row).
CREATE TABLE IF NOT EXISTS referral_settings (
  id                INTEGER PRIMARY KEY CHECK (id = 1),
  agent_signup_open BOOLEAN NOT NULL DEFAULT true,
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_by        UUID
);
INSERT INTO referral_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS referral_settings;
DROP INDEX IF EXISTS ux_referral_codes_agent_owner;
DROP INDEX IF EXISTS ux_referral_codes_payout;
ALTER TABLE referral_codes DROP COLUMN IF EXISTS payout_name;
ALTER TABLE referral_codes DROP COLUMN IF EXISTS payout_account;
ALTER TABLE referral_codes DROP COLUMN IF EXISTS payout_bank;
ALTER TABLE referral_codes DROP COLUMN IF EXISTS payout_method;
