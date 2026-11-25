# Referrals (agents and influencers)

Each agent or influencer gets a referral code. The code's link is `https://<app>/?ref=CODE`, or `https://t.me/<bot>?startapp=ref_CODE` for the Telegram mini app. The backend tracks the funnel from link visit to signup to qualified bidder. It also pays the code's owner per qualified bidder under payout rules that staff set in the admin panel.

## Funnel

1. **Visit.** The frontend posts `POST /api/public/referrals/:code/click` with an anonymous `visitorId` that it stores in localStorage. There is one row per code and visitor, and repeat opens increase `hits`. "Visitors" counts unique visitors and "visits" counts all opens.
2. **Signup.** After signup or login, the frontend posts `POST /api/referrals/claim`. A claim counts only when all of these are true:
   - The account is a participant.
   - The account is under 72 hours old.
   - The account has never paid for a ticket.
   - The account has not been referred before. The first code wins.
   - The code is active.
   - The code doesn't belong to the same person, judged by owner account or phone.
3. **Qualified bidder.** A background sweep runs every 2 minutes, and right after staff changes. It promotes a signup to qualified when all of these are true:
   - The account made a **successful `entry` payment with amount > 0** on a qualifying auction. Leave the list empty to count any auction.
   - The payment came within the program's attribution window after signup. The default is 30 days.
   - The account has a phone number.
   - No other qualified referral used the same phone.
   - No other qualified referral used the same payment account, as reported by Chapa.

### Why "paid entry ticket" is the definition

Under pay-on-win, a bidder pays the entrance fee for every ticket, and only the winner pays the bid amount. The entrance fee is the money the platform actually collects from every bidder, so a referral is worth paying for once that money arrives.

Requiring a winning payment would reward luck rather than the agent's work. Free-entrance tickets never count, because they bring in no money. A refunded ticket stops counting at the next sweep.

### Anti-fraud

- Each phone number counts once across all codes. A partial unique index on `referral_attributions(qualified_phone)` enforces this.
- Each payment account counts once across all codes. A partial unique index on `qualified_account` enforces this. Chapa doesn't always return the payer's account. When it doesn't, this check is skipped and the phone check still applies.
- Self-referrals are rejected. This covers the owner's account and the owner's phone.
- A signup from a browser that other signups on the same code also used is flagged to staff as `sameDevice`. This is a hint for review, not an automatic rejection.
- Staff can reject a referral with a note, and restore it later. Both actions are audit-logged.

## Payouts

Each code uses either the default program for its kind or a custom program. A program has these settings:

| Setting | Default | Meaning |
| --- | --- | --- |
| `threshold` | 10 | Qualified bidders needed before anything is paid. |
| `tiers` | 11–25: 5 ETB, 26–50: 10 ETB, 51+: 20 ETB | Pay per bidder. Each bidder is paid the rate of the tier that its number falls in. |
| `netMarginPerBid` | 25 ETB | What the platform keeps from one paid ticket. |
| `attributionWindowDays` | 30 | Days after signup allowed for the first paid ticket. |
| `qualifyingAuctionIds` | empty (any) | Limits which tickets count, for example the power bank auction. |

Validation is the guardrail:

- Tiers start right after the threshold.
- Tiers have no gaps.
- Pay never decreases from one tier to the next.
- The last tier is open-ended.
- **Every tier pays strictly less than `netMarginPerBid`.**

If the cheapest qualifying ticket costs less than the margin, the staff UI shows a warning.

Earnings are computed live from the current rules and the current count of qualified bidders, so a rule change applies to past bidders too. Staff record payouts by hand, after paying through telebirr, a bank or cash. `owed` = `earned` − `paidOut`. `overpaid` is shown when a refund drops a qualified bidder after that bidder was paid out.

## API for the agent dashboard

All of these endpoints are signed in and return the usual `{ success, message, data }` envelope. Money values are numbers in ETB. Rates are percentages from 0 to 100.

### `GET /api/referrals/me`

```jsonc
{
  "isReferrer": true,              // false when this account owns no code
  "codes": [{
    "code": { "id", "code", "kind", "label", "status", "link", "createdAt" },
    "program": { "id", "name", "kind", "isDefault", "threshold", "tiers": [{ "from", "to?", "amount" }],
                 "attributionWindowDays", "qualifyingAuctions": [{ "id", "title" }] },
    "funnel": { "visitors", "visits", "signups", "pending", "qualified", "rejected",
                "visitorToSignupRate", "signupToQualifiedRate" },
    "earnings": { "qualified", "threshold", "unlocked", "remaining", "earned", "currentRate",
                  "nextTier?": { "from", "amount", "biddersNeeded" },
                  "tiers": [{ "from", "to?", "amount", "bidders", "subtotal" }],
                  "paidOut", "owed", "overpaid" }
  }],
  "totals": { "funnel": { … }, "earned", "paidOut", "owed" }
}
```

### Other endpoints

- **`GET /api/referrals/me/referrals?status=signed_up|qualified|rejected&page&limit&codeId`** returns a paged list of the people this account referred. Names and phones are masked (`Abebe K.`, `•••• ••44`). Each item has these fields: `status`, `signedUpAt`, `qualifiedAt`, `qualifiesBy` (the end of the window), `expired`, `needsPhone`, `auctionTitle` and `rejectReason`.
- **`GET /api/referrals/me/activity?days=30&codeId`** returns daily `{ day, visitors, signups, qualified }`. Days are in Africa/Addis_Ababa time and missing days are zero-filled. The maximum is 90 days.
- **`GET /api/referrals/me/payouts`** returns the payouts staff recorded: `{ amount, method, reference, note, paidAt }`.

## Public endpoints

- `GET /api/public/referrals/:code` returns `{ code, kind, invitedBy }`. The frontend uses it for the "Invited by Abebe K." banner on signup.
- `POST /api/public/referrals/:code/click` takes `{ visitorId, landingPath? }`. It is rate-limited to 300 per minute per IP and code.
- `POST /api/referrals/claim` (signed in) takes `{ code, visitorId? }` and returns `{ attributed, code, reason? }`. The reason is one of `already_referred`, `existing_account`, `existing_customer` or `not_participant`.

## Staff endpoints

Admin, super_admin and compliance can read. Only admin and super_admin can write.

- `GET /api/referral-programs`: each program also includes `netMarginPerBid`, `topTierAmount`, `lowestEntranceFee` and `warnings`.
- `POST /api/referral-programs`: creates a custom program.
- `PATCH /api/referral-programs/:id`
- `GET /api/referral-codes?kind&status&search&page&limit`
- `POST /api/referral-codes` takes `{ kind, ownerPhone, code?, label?, programId? }`. An agent code needs an existing account with that phone. If `code` is left out, a 6-character code is generated.
- `GET /api/referral-codes/:id`
- `PATCH /api/referral-codes/:id` takes `{ label?, programId?, clearProgram?, status? }`.
- `GET /api/referral-codes/:id/referrals`: the list is unmasked and includes `sameDevice`.
- `GET /api/referral-codes/:id/activity` and `GET /api/referral-codes/:id/payouts`
- `POST /api/referral-codes/:id/payouts` takes `{ amount, method?, reference?, note? }`. The amount can't exceed what is owed.
- `POST /api/referral-attributions/:id/reject` takes `{ note? }`.
- `POST /api/referral-attributions/:id/restore`
- `POST /api/referrals/sweep`: runs the qualification check now.

## Influencer codes (Phase 2)

Influencer codes are the same codes with `kind: "influencer"` and their own default program. An owner account is optional, so a code can be tracked before the influencer signs up. To give one partner special rates, create a custom program and assign it to that partner's code.

## Schema

The tables are `referral_programs`, `referral_codes`, `referral_clicks`, `referral_attributions` and `referral_payouts`. They are created idempotently by `database.EnsureSafeSchema` (`platform/database/referrals.go`) and mirrored in `migrations/00018_referrals.sql`. A default program for each kind is seeded on first start.
