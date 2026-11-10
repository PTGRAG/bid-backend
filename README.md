# Carra Market — Backend

Go API for Carra Market auctions: LUB (lowest unique bid) products, Chapa payments, Telegram + SMS notifications, staff ops.

Module: `github.com/bid-hub/admin-backend`

**Project docs:** [../docs/README.md](../docs/README.md) (architecture, payments, local setup, deploy).

## Stack

- Go 1.26, Fiber v3
- PostgreSQL (GORM + SQL migrations under `platform/database/migrations`)
- Redis (optional / cache helpers)
- Chapa payment gateway
- Telegram Bot API (channel posts, admin DMs, Mini App auth)
- AfroMessage SMS
- Sentry + Prometheus (`GET /metrics`)

## Setup

```bash
cp .env.example .env
# set DB_URL / DATABASE_URL, JWT secrets, Chapa, Telegram, etc.

go mod tidy
make run
# equivalent: go run cmd/api/main.go
```

API defaults to `APP_PORT` (8080). Health: `GET /health`.

### Key env vars

| Variable | Purpose |
|----------|---------|
| `DB_URL` / `DATABASE_URL` | Postgres DSN |
| `JWT_ACCESS_SECRET` / `JWT_REFRESH_SECRET` / `CSRF_SECRET` | Auth (min 32 chars in prod) |
| `BOT_TOKEN` / `BOT_USERNAME` | Telegram bot |
| `CHANNEL_ID` | Channel for auction posts |
| `ADMIN_ID` | Admin chat for payment success/failure alerts |
| `CHAPA_SECRET_KEY` / `CHAPA_PUBLIC_KEY` | Payments |
| `CALLBACK_URL` | Public HTTPS Chapa webhook (omit on localhost; return URL + reconciler fulfill) |
| `AFRO_MESSAGE_*` | Buyer SMS on ticket purchase / payment failure |
| `SENTRY_DSN` | Error + payment issue capture |
| `PARTICIPANT_APP_URL` | Frontend origin for return URLs / links |

See `.env.example` for the full list and comments.

## Payments (Chapa)

Flow overview:

1. Client starts participation → backend creates a **pending** payment ledger row and Chapa checkout (entrance fee ± bid pack).
2. Chapa **webhook** / **return URL verify** / background **reconciler** call verify and run `ProcessVerified`.
3. On success: mark payment `success`, record **participation**, place any pending bid pack, notify buyer (in-app + SMS if phone) and admin (Telegram).

Notes:

- Loopback `CALLBACK_URL` is omitted when talking to Chapa (Chapa rejects localhost); local fulfillment relies on return URL + reconciler.
- Fulfillment is idempotent (safe to re-verify a successful `tx_ref`).
- Staff UI can monitor payments / pending Chapa / alerts.

One-off repair tool (ops): `go run ./cmd/fixpay [payment-id|tx_ref]` — forces verify/fulfill against the current `DB_URL`. Prefer production DSN only when intentional.

## Public auction API (frontend cards)

List/detail payloads include `title`, `description`, `entranceFee`, `retailValue`, media, status, countdown fields, etc.

Frontend cards currently surface the **first sentence of `description`** and hide participant counts / progress bars in the UI (backend still returns counts for staff/analytics).

## Layout

| Path | Role |
|------|------|
| `cmd/api` | HTTP server entrypoint |
| `adapters/handler/http` | Fiber routes & handlers |
| `adapters/repository/postgres` | Persistence |
| `adapters/payment` | Chapa client |
| `core/domain` | Entities |
| `core/services` | LUB, Chapa payment, notifications, auth |
| `platform/` | Shared env, DB, validate, logger (see `platform/README.md`) |
| `integration/` | Integration tests |

## Scripts

| Command | Description |
|---------|-------------|
| `make build` | `go build ./...` |
| `make run` | Run API |
| `make tidy` | `go mod tidy` |
| `go test ./...` | Unit tests |
| `go test -race ./...` | Race-enabled tests (CI) |

## Docker / deploy

- Multi-stage `Dockerfile`
- CI builds **`linux/arm64`** on native ARM runners for AWS Graviton
- Configure secrets via runtime env / compose — do not bake secrets into the image

## Related

- Frontend: `../bid-frontend`
- Migrations notes: `platform/database/migrations/README.md`
- Platform packages: `platform/README.md`
