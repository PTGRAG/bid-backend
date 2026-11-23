-- +goose Up
-- Pay only if you win: tickets cost the entrance fee only; after the draw the winner gets 48 hours
-- to pay their bid amount, otherwise the prize passes to the next lowest unique bid (max 3 offers).

CREATE TABLE IF NOT EXISTS auction_winner_offers (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  auction_id     UUID NOT NULL REFERENCES auctions(id) ON UPDATE CASCADE ON DELETE CASCADE,
  bid_id         UUID NOT NULL REFERENCES auction_bids(id) ON UPDATE CASCADE ON DELETE RESTRICT,
  participant_id UUID NOT NULL,
  bid_value      DECIMAL(10,2) NOT NULL,
  attempt        INTEGER NOT NULL,
  status         VARCHAR(20) NOT NULL DEFAULT 'awaiting_payment',
  due_at         TIMESTAMPTZ NOT NULL,
  reminders_sent INTEGER NOT NULL DEFAULT 0,
  paid_at        TIMESTAMPTZ,
  payment_tx_ref VARCHAR(255),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_winner_offers_status CHECK (status IN ('awaiting_payment', 'paid', 'expired')),
  CONSTRAINT chk_winner_offers_attempt CHECK (attempt BETWEEN 1 AND 3),
  CONSTRAINT chk_winner_offers_reminders CHECK (reminders_sent BETWEEN 0 AND 2)
);

CREATE INDEX IF NOT EXISTS idx_winner_offers_auction ON auction_winner_offers (auction_id);
CREATE INDEX IF NOT EXISTS idx_winner_offers_open_due ON auction_winner_offers (due_at) WHERE status = 'awaiting_payment';
CREATE UNIQUE INDEX IF NOT EXISTS ux_winner_offers_attempt ON auction_winner_offers (auction_id, attempt);
-- One open offer and one paid offer per auction; a participant is offered an auction at most once.
CREATE UNIQUE INDEX IF NOT EXISTS ux_winner_offers_one_open ON auction_winner_offers (auction_id) WHERE status = 'awaiting_payment';
CREATE UNIQUE INDEX IF NOT EXISTS ux_winner_offers_one_paid ON auction_winner_offers (auction_id) WHERE status = 'paid';
CREATE UNIQUE INDEX IF NOT EXISTS ux_winner_offers_participant ON auction_winner_offers (auction_id, participant_id);

ALTER TABLE payments
  ADD COLUMN IF NOT EXISTS purpose VARCHAR(20) NOT NULL DEFAULT 'entry',
  ADD COLUMN IF NOT EXISTS winner_offer_id UUID;
ALTER TABLE payments
  ADD CONSTRAINT chk_payments_purpose CHECK (purpose IN ('entry', 'winner'));
CREATE INDEX IF NOT EXISTS idx_payments_winner_offer ON payments (winner_offer_id);

-- A single free ticket now costs nothing, so a participation may record 0 paid.
ALTER TABLE auction_participations DROP CONSTRAINT IF EXISTS chk_participations_fee_paid;
ALTER TABLE auction_participations ADD CONSTRAINT chk_participations_fee_paid CHECK (fee_paid >= 0);

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS notify_results_email BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS notify_results_telegram BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users
  DROP COLUMN IF EXISTS notify_results_telegram,
  DROP COLUMN IF EXISTS notify_results_email;

ALTER TABLE auction_participations DROP CONSTRAINT IF EXISTS chk_participations_fee_paid;
ALTER TABLE auction_participations ADD CONSTRAINT chk_participations_fee_paid CHECK (fee_paid > 0) NOT VALID;

DROP INDEX IF EXISTS idx_payments_winner_offer;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_purpose;
ALTER TABLE payments
  DROP COLUMN IF EXISTS winner_offer_id,
  DROP COLUMN IF EXISTS purpose;

DROP TABLE IF EXISTS auction_winner_offers;
