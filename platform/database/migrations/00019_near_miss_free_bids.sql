-- +goose Up
-- Near-miss free bids: once an auction's winner has paid, the next lowest unique bids (and,
-- optionally, duplicate knockouts below the winner) get one free bid each. A free bid waives one
-- ticket's entrance fee at a later checkout; payments.free_bids_used records the spend.

CREATE TABLE IF NOT EXISTS free_bid_grants (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  participant_id UUID NOT NULL,
  auction_id     UUID REFERENCES auctions(id) ON DELETE SET NULL,
  reason         VARCHAR(30) NOT NULL CHECK (reason IN ('runner_up', 'duplicate_knockout', 'staff_grant')),
  rank           INTEGER NOT NULL DEFAULT 0,
  bid_value      DECIMAL(10,2),
  note           VARCHAR(255) NOT NULL DEFAULT '',
  created_by     UUID,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- One near-miss free bid per customer per auction: settling twice never credits twice.
CREATE UNIQUE INDEX IF NOT EXISTS ux_free_bid_grants_auction ON free_bid_grants (participant_id, auction_id) WHERE auction_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_free_bid_grants_participant ON free_bid_grants (participant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS near_miss_settings (
  id                 INTEGER PRIMARY KEY CHECK (id = 1),
  enabled            BOOLEAN NOT NULL DEFAULT true,
  runner_ups         INTEGER NOT NULL DEFAULT 4 CHECK (runner_ups BETWEEN 0 AND 50),
  include_duplicates BOOLEAN NOT NULL DEFAULT false,
  max_duplicates     INTEGER NOT NULL DEFAULT 10 CHECK (max_duplicates BETWEEN 0 AND 500),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_by         UUID
);

ALTER TABLE payments ADD COLUMN IF NOT EXISTS free_bids_used INTEGER NOT NULL DEFAULT 0;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_free_bids_used;
ALTER TABLE payments ADD CONSTRAINT chk_payments_free_bids_used CHECK (free_bids_used >= 0);

-- +goose Down
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_free_bids_used;
ALTER TABLE payments DROP COLUMN IF EXISTS free_bids_used;
DROP TABLE IF EXISTS near_miss_settings;
DROP TABLE IF EXISTS free_bid_grants;
