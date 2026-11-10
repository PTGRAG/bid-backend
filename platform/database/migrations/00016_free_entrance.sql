-- +goose Up
-- Free-entrance promo (admin-controlled per auction). When enabled, the first N joiners skip the
-- entrance fee but still pay their bid amount via Chapa. Limit 0 means "all".
ALTER TABLE auctions
  ADD COLUMN IF NOT EXISTS free_entrance_enabled BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS free_entrance_limit INTEGER NOT NULL DEFAULT 100,
  ADD COLUMN IF NOT EXISTS free_entrance_claimed INTEGER NOT NULL DEFAULT 0;

ALTER TABLE auctions
  ADD CONSTRAINT chk_auctions_free_entrance CHECK (
    free_entrance_limit >= 0 AND free_entrance_limit <= 1000000
    AND free_entrance_claimed >= 0
    AND (free_entrance_limit = 0 OR free_entrance_claimed <= free_entrance_limit)
  );

ALTER TABLE auction_participations
  ADD COLUMN IF NOT EXISTS entrance_free BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE payments
  ADD COLUMN IF NOT EXISTS entrance_waived BOOLEAN NOT NULL DEFAULT false;

-- Each customer may use free entrance on only one auction (ever): at most one pending/successful
-- free-entrance checkout per customer, and at most one active free participation per customer.
CREATE UNIQUE INDEX IF NOT EXISTS ux_payments_one_waived_per_user
  ON payments (user_id)
  WHERE entrance_waived = true AND status IN ('pending', 'success');

CREATE UNIQUE INDEX IF NOT EXISTS ux_participation_one_free_per_user
  ON auction_participations (participant_id)
  WHERE entrance_free = true AND status = 'active';

-- +goose Down
DROP INDEX IF EXISTS ux_participation_one_free_per_user;
DROP INDEX IF EXISTS ux_payments_one_waived_per_user;

ALTER TABLE payments DROP COLUMN IF EXISTS entrance_waived;
ALTER TABLE auction_participations DROP COLUMN IF EXISTS entrance_free;

ALTER TABLE auctions DROP CONSTRAINT IF EXISTS chk_auctions_free_entrance;
ALTER TABLE auctions
  DROP COLUMN IF EXISTS free_entrance_enabled,
  DROP COLUMN IF EXISTS free_entrance_limit,
  DROP COLUMN IF EXISTS free_entrance_claimed;
