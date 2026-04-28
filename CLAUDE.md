# Freefall — project memory for Claude

> Read this first.

## What this is

Multi-tenant SaaS for tandem-skydive operators. Operator films 7 canonical segments + arbitrary custom segments → system auto-edits + adds music with sidechain ducking → delivers to the client via a personal portal (`/watch/<access_code>`) with optional paid photo pack.

**Two binaries from one repo:**

- **`cmd/server`** — cloud HTTP (Hetzner Cloud). Tenants, operators, music library, client portal, Stripe + Montonio webhooks, Flowtark monthly billing cron.
- **`cmd/studio`** — local Windows native binary. HTTP UI at `localhost:8080`, FFmpeg pipeline (10 stages), uploads to the tenant's S3-compatible storage.

Full plan: `~/.claude/plans/https-freefall-ing-giggly-mist.md`.

## Tech stack (locked in)

- Go 1.22+ (currently developed against 1.26)
- PostgreSQL 16 via `pgx/v5`, raw SQL, migrations via `golang-migrate`
- Redis (sessions on cloud)
- FFmpeg (system binary on PATH for dev; bundled in installer for distribution)
- GoCV / OpenCV — face-detect + motion-magnitude in studio
- S3 SDK (`aws-sdk-go-v2`) — works against Hetzner OS, MinIO, Backblaze B2, AWS
- `chi` HTTP router
- Stripe + Montonio (pluggable `Provider` interface in `internal/billing`)
- Flowtark API for monthly B2B invoices to clubs
- Caddy for production TLS
- Sentry (`sentry-go`)

## Distribution stages

1. **Stage 1 (now)**: `go run` locally on dev machine
2. **Stage 2 (alpha)**: native unsigned `studio.exe` on Windows; operator clicks through SmartScreen "More info → Run anyway"
3. **Stage 3 (production)**: Apple Developer ID ($99/yr) + Windows EV cert ($300/yr) when first paying customers arrive

Mac builds deferred — owner has no Mac yet.

## Port map (dev)

| Service | Host port |
|---|---|
| Cloud server | 8000 |
| Studio (local UI) | 8080 |
| Postgres | 55432 |
| Redis | 56379 |
| MinIO API | 59000 |
| MinIO Console | 59001 |

## How to run

```powershell
docker compose up -d
go run ./cmd/server migrate up
go run ./cmd/server         # cloud, :8000
go run ./cmd/studio          # local, :8080
```

## File layout

See `README.md` for the canonical tree. Highlights:

- `cmd/server/main.go` — chi router, sentry init, healthz
- `cmd/studio/main.go` — local HTTP server, opens browser to :8080 on launch
- `internal/api/v1/*.go` — DTO contract (jump, music, license, storage, upload). Versioned. Don't break wire compatibility within v1.
- `internal/db/db.go` — pgx pool, transaction helpers
- `internal/billing/provider.go` — Stripe + Montonio behind one interface
- `migrations/0001_init.up.sql` — full schema (tenants, operators, jumps, jump_artifacts, music_tracks, photo_orders, usage_events, monthly_invoices, tenant_storage_configs, tenant_payment_configs, processed_webhook_events, app_settings)

## Conventions

- **Raw SQL** only (`%s` placeholders ignored — pgx uses `$1, $2, …`)
- **Tenant scoping** on every query: `WHERE tenant_id = $1`
- **No ORM**. No code generation. No Wire/DI framework.
- **API contract** changes ship in `internal/api/v1` only; don't break field names/types within v1
- **Webhooks**: idempotent via `processed_webhook_events (provider, event_id)` PK
- **Plan writes to `tenants.plan` happen only in webhook handlers** (mirrors Flowtark discipline)
- **Photo originals** never accessible via predictable URL — UUID + salt in S3 key, presigned only on click after payment

## Memory aids

- Owner email: `sergei@youngearlystarters.com`
- Sister project: Flowtark (https://flowtark.com) — invoice SaaS at `C:\Users\serge\OneDrive\Documents\Projects\invoice system`. Freefall sends monthly B2B bills via Flowtark's API.
- B2C Photo pack default price: €29 (`tenants.end_customer_photo_price_cents = 2900`)
- B2B per-jump default: €5 (`tenants.video_price_cents = 500`)
- Access code format: 8 chars base32 minus `0/O/1/I` (~10⁹ possibilities)
