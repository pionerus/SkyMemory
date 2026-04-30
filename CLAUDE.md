# Skydive Memory — project memory for Claude

> Read this first. Updated 2026-04-30.
>
> **Canonical roadmap lives in `~/.claude/plans/skydive-memory-master-plan.md`.**
> When this file disagrees with the master plan, the master plan wins.
> Phase-state snapshots may also live in `~/.claude/projects/.../memory/`.

## What this is

Multi-tenant SaaS for tandem-skydive operators ("clubs"). Operators film 7
canonical segments (intro / interview_pre / walk / interview_plane / freefall
/ landing / closing), the studio app auto-edits them with music + sidechain
ducking + watermark + intro/outro, and delivers up to 6 video deliverables
+ photos to the jumper via a `/watch/<access_code>` portal.

Brand: **Skydive Memory** (was "Freefall" — repo + binaries kept the old
name; only user-facing strings rebranded). Domain target
`skydivememory.app`, slug pattern `<club>.skydivememory.app`. Brand color
`#FFC83D` (yellow), accent `#FF8A3D` (orange), squirrel-mask logo, Plus
Jakarta Sans, dark theme only.

### Four portals (NOT two)

1. **Platform admin** (`/platform/*`) — us. Cross-tenant ops, club CRUD,
   per-club pricing, billing aggregation. Auth: `platform_admins` table,
   bootstrap via `bin/server.exe platform-admin add`.
2. **Club admin** (`/`) — `operators` with `role='owner'`. Music library,
   branding, jump records, tokens.
3. **Operator portal** (`/operator/*`) — `operators` with `role='operator'`.
   Web mirror of studio.exe state, personal storage configs (Drive/Dropbox/
   OneDrive). Skeleton only at this point.
4. **Client view** — public `/watch/<access_code>` + magic-link `/me/<token>`
   for cross-jump access (`client_email_links` table). Route placeholder
   only at this point.

`/auth/login` JS redirects by role: `owner → /`, `operator → /operator/`,
`platform → /platform/`. `auth.SessionData.PlatformAdminID` and
`OperatorID` are mutually exclusive — `Set` and `SetPlatformAdmin` clear
each other.

### Two Go binaries from one repo

- **`cmd/server`** — cloud HTTP. Goes to Hetzner Cloud (deferred). Repo +
  module path is still `github.com/pionerus/freefall`; don't refactor that.
- **`cmd/studio`** — Windows-native, runs on each operator's machine. HTTP
  UI on `localhost:8080`, drives the FFmpeg pipeline. Talks to cloud over
  HTTPS using a Bearer license token. Stays untouched as a desktop app —
  the operator web portal is *additional*, not a replacement.

Repo: https://github.com/pionerus/SkyMemory

## Tech stack

- **Go 1.26.2** (`C:\Program Files\Go\bin\go.exe` — not on PATH; use the
  full path or `run-*.bat`).
- PostgreSQL 16 via `pgx/v5`, raw SQL, migrations via `golang-migrate`
  (cloud). Studio uses its own SQLite via `modernc.org/sqlite` (pure-Go,
  no cgo).
- Redis (cloud session store via `gorilla/sessions`).
- FFmpeg + ffprobe — system binaries; in dev they're at
  `C:\Users\serge\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_…\bin\`
  (not on PATH; refresh User PATH in a fresh shell before
  `.\run-studio.bat`).
- `chi` HTTP router. `aws-sdk-go-v2` for S3 (Hetzner OS / MinIO / B2
  compatible). `bcrypt` (cost 12) for passwords; SHA-256 hex for license
  token storage.
- Caddy + TLS, Sentry — production deploy phase, not yet wired.

## Distribution stages

1. **Stage 1 (NOW)**: dev box only. Local PowerShell.
2. **Stage 2 (alpha)**: ship `studio.exe` unsigned to friendly operators
   — they click past Windows SmartScreen on first launch.
3. **Stage 3 (paying customers)**: Apple Dev ID ($99/yr) + Windows EV cert
   (~$300/yr).

**Mac is deferred** — owner has no Mac.

## Port map (dev)

| Service | Host port | Notes |
|---|---|---|
| Cloud server | **8000** | server.exe |
| Studio | **8080** | studio.exe |
| Postgres | **55433** | NOT 55432 — Flowtark's Postgres squats 55432 with a different `freefall_dev` password. Auth failures look like "wrong creds" but are actually the wrong DB. |
| Redis | 56379 | internal-only in prod |
| MinIO S3 API | 59000 | swap to Hetzner Object Storage in prod |
| MinIO Console | 59001 | http://localhost:59001 — `freefall` / `freefall_dev_secret` |

S3 buckets:
- `freefall-music` — music library
- `freefall-branding` — per-tenant watermark PNG + intro/outro clips
  (server.exe auto-creates on boot)

## How to run (dev)

```powershell
cd "C:\Users\serge\OneDrive\Documents\Projects\Freefall video"
docker compose up -d              # postgres + redis + minio
.\run-server.bat                  # window 1: cloud :8000
.\run-studio.bat                  # window 2: studio :8080
```

`run-server.bat` and `run-studio.bat` parse `.env` (skip `#` lines), build
`bin/<name>.exe` if missing via the absolute Go path, and launch.

`migrate.bat <up|down|version>` runs the cloud migrations subcommand.

**Loading `.env` in a bash session** (no `export` prefix in the file):

```bash
set -a && source <(grep -v '^#' .env | grep -v '^$' | sed 's/^/export /') && set +a
```

**Bootstrap a platform admin** (one-time per dev box):

```powershell
bin\server.exe platform-admin add ops@skydivememory.app "Ops"
# password prompt — Scanln, echoes plaintext, fine for dev
```

Existing dev account: `ops@skydivememory.app` / `skydive-dev-2026`.

**Smoke check after restart:**

- http://localhost:8000/auth/login — login (redirects by role)
- http://localhost:8000/platform/ — platform admin dashboard
- http://localhost:8000/ — club admin dashboard
- http://localhost:8000/admin/tokens — issue a license token (paste into
  `.env` `STUDIO_LICENSE_TOKEN`, restart studio)
- http://localhost:8000/admin/music-library — upload real MP3s (NOT
  random bytes — fakes won't decode in browser preview)
- http://localhost:8000/admin/branding — watermark + intro/outro upload
- http://localhost:8080 — studio dashboard

## Schema state

**Cloud Postgres** at v3:

- **0001_init**: tenants, operators, license_tokens, clients, jumps,
  jump_artifacts, music_tracks, music_visible_to view, monthly_invoices,
  usage_events, tenant_payment_configs, tenant_storage_configs,
  photo_orders, processed_webhook_events, app_settings.
- **0002_tenants_branding**: `tenants` extended with `watermark_size_pct`,
  `watermark_opacity_pct`, `watermark_position`, `intro_clip_path`,
  `outro_clip_path`.
- **0003_4portal**: `platform_admins`, `operator_storage_configs`,
  `tenant_sepa_configs`, `client_email_links`, `watch_events`,
  `jump_terms`. `jumps` extended with `deliverables_email_sent_at`,
  `deliverables_email_message`.

`jump_artifacts.kind` enum supports up to 6 video deliverables per jump:
`video_main`, `video_instagram`, `video_wow_highlights`, `video_plane_exit`,
`video_freefall`, `video_operator_flyby`. Pipeline currently only emits
`video_main` — multi-deliverable is Phase 5 in the master plan.

**Studio SQLite** at v6 (auto-migrated on Open at
`~/.freefall-studio/state.db`):

- v1 projects, v2 clips, v3 clip trim columns, v4 projects music columns,
  v5 generations, v6 `clip_cuts` (CUT/exclude feature).

## What's built (high level — verify against code before relying)

**Done:**

- **Phase 0** baseline pipeline: single-pass with QSV, fade-in/out, smooth
  progress, RunRegistry+Stop, sidechain music ducking.
- **Phase A** brand pass: dark-theme tokens applied to all old templates,
  squirrel-mark logo via mask-PNG (`mask-image` + `currentColor`), admin
  shell rail, dashboard stub, settings modal, auth polish.
- **Phase 1** 4-portal foundation: schema migrations 0002+0003, studio
  v6 migration, 4-role auth + middleware (`RequirePlatformAdmin`),
  platform admin CLI, platform login flow, /platform/ + /operator/
  skeleton dashboards, login redirect by role.
- **Phase 2** wizard split: project creation flow is now 3 screens —
  `/projects/new` (POST creates project) → `/projects/{id}/clips`
  (clips + trim + cuts + music) → `/projects/{id}/generate` (final
  ffmpeg + email placeholder). Old `/projects/{id}` is a 302 redirect
  to `/clips` for backwards-compat.
- **Phase 3.1 + 3.2 + 3.4** project_detail rewrite: sticky topbar with
  click-to-copy access code, 4-column clip-board grid (every clip
  always-open, no modal), refreshed music card with mood-gradient cover.
- **Phase 3.3 (CUT)**: `clip_cuts` table + DB methods + HTTP endpoints
  (POST/DELETE/GET) + multi-zone painter UI + pipeline split+concat
  integration (`writeClipVideoChain` / `writeClipAudioChainFromSource`).
- **Phase 6 admin side**: `/admin/branding` real UI + cloud endpoints
  for watermark + intro/outro upload/delete + settings,
  `internal/branding` package, separate `freefall-branding` bucket.

**In flight / next** (per master plan §3):

- **Phase 6 part 2** (NEXT): pipeline overlay filter for watermark +
  intro/outro concat + studio-side fetch & cache via new
  `/api/v1/tenant/branding` endpoint. Renders today still come out without
  watermark — UI saves settings but pipeline ignores them.
- **Phase 4** Timeline preview + MSE player.
- **Phase 5** Multi-deliverable pipeline (6 video outputs per jump).
- **Phase 7** Watch page `/watch/<code>`.
- **Phase 9** Operator portal real impl.
- **Phase 10** Platform admin clubs CRUD (currently 4 stat cards work,
  rest is skeleton).
- **Phase 11** Club admin extensions: real dashboard queries, Storage/
  Clients/Jumps pages, SEPA UI.
- **Phase 12** Billing aggregation.
- **Phase 13** Email send post-render — Resend integration over the
  Skydive Memory domain.

## Studio FFmpeg pipeline (`internal/studio/pipeline/runner.go`)

Single-pass per generation:

- **Stage A** trim+normalise: per-clip → 1920×1080 30fps H.264 + AAC
  stereo 48k. Action kinds (intro/walk/freefall/landing/closing/custom)
  get their audio replaced by `anullsrc` silence. Interview kinds keep
  speech. CUT zones split into segments and concatenated.
- **Stage B** crossfades: one ffmpeg with N inputs + filter_complex
  `xfade` (video) + `acrossfade` (audio), 0.5s default crossfade,
  shrunk to `clip_duration/3` for short clips. Single-clip projects
  skip. Returns the actual post-crossfade duration.
- **Stage C** music mix (skipped if `music_track_id=0`): looped, faded
  in 1s, pre-attenuated 0.7×; sidechain compressor with project audio
  as the duck driver (silence in action segments → no duck → music
  plays full; speech in interview segments → ratio 8 duck → speech is
  clear); 1s afade-out at the real concat-end timestamp.
- **Stage D** stat → `status='done'` + `output_path` + `output_size`.

QSV reality on this dev box: Intel UHD 620 iGPU, `h264_qsv` works with
`-low_power 1`, but only ~1.25× realtime end-to-end because xfade is
CPU-bound. RunRegistry serializes generations to avoid QSV deadlock from
parallel encodes on the single iGPU engine.

## Repo discipline

- Raw SQL only (no ORM). pgx uses `$1, $2…` placeholders.
- Multi-tenant scoping on every cloud query: `WHERE tenant_id = $1`.
- API contract lives in `internal/api/v1`; don't break field names/types
  within v1.
- Webhook handlers (later): idempotent via `processed_webhook_events
  (provider, event_id)` PK.
- Photo originals (later): UUID+salt in S3 key, presigned 15-min only
  after payment confirms.
- `tenants.plan` writes (later): only by Stripe webhook handler.
- All `Write`-ed Go files run `go vet ./...` clean.
- **Don't add `tenant_id` to `platform_admins`** — they're cross-tenant
  by design.
- **Don't refactor module path / binary names** away from "freefall" —
  git history + import paths not worth the churn. Only user-facing
  strings are rebranded to "Skydive Memory".

## Memory aids

- Owner: sergei@youngearlystarters.com (Claude context),
  sergei.zamsharski@gmail.com (GitHub `pionerus`).
- Sister project: **Flowtark** (https://flowtark.com — invoice SaaS at
  `C:\Users\serge\OneDrive\Documents\Projects\invoice system`).
  Skydive Memory will send monthly B2B bills via Flowtark's API
  (Phase 12, not yet wired).
- B2C photo pack default: €29 (`tenants.end_customer_photo_price_cents`).
- B2B per-jump default: €5 (`tenants.video_price_cents`).
- Access code format: 8 chars **Crockford Base32** (`0-9A-Z` minus
  `I/L/O/U`) → ~10¹² combos. Stored canonical (no dash) in DB; rendered
  as `XXXX-XXXX`.
- Brand assets: `internal/studio/ui/static/squirrel-mark.png` and
  `web/server/templates/static/squirrel-mark.png` — used via CSS
  `mask-image` (`.sm-mark` class) so they inherit `currentColor`.
  Token list in `internal/studio/ui/static/skydive-memory.css`.
- Old palette (indigo/peach/lavender pastels:
  `#4f46e5 / #ffd6b3 / #c5deff / #e0d4ff`) is gone everywhere except
  buried inline `<style>` in `project_detail.html` overridden via a
  dark-theme override layer at the end of that block.

## Common gotchas (paid for in tears already)

- **Stale processes**: `kill $PID` in bash often leaks
  `server.exe` / `studio.exe` zombies. Check first:
  ```powershell
  Get-Process server,studio,ffmpeg -EA SilentlyContinue
  Get-NetTCPConnection -LocalPort 8000,8080
  Get-Process server,studio,ffmpeg -EA SilentlyContinue | Stop-Process -Force
  ```
- **bash on Windows + PATH**: `export PATH=` won't work with `C:/...`
  paths; use `/c/Users/...` form.
- **Working dir** sometimes is `…\Freefall video` and sometimes
  `…\Freefall video\build`. Bash tool resets cwd between calls — either
  prepend `cd "/c/Users/serge/OneDrive/Documents/Projects/Freefall video"`
  or use absolute paths.
- **Python in inline test scripts**: `python` not `python3` on this
  machine.
- **Postgres 55432 vs 55433**: see port-map table — Flowtark squats
  55432.
- **MP3 fakes**: `head -c 12345 /dev/urandom > track.mp3` is a 12 KB
  random-byte file, NOT decodable. Browser audio preview silently fails.
  Use `ffmpeg -f lavfi -i sine=…` or actual royalty-free downloads.
- **ffmpeg-on-PATH for studio**: studio's process inherits PATH from the
  shell that launched it. Updating User PATH after install requires a
  fresh PowerShell window before `.\run-studio.bat`.
- **Go HTTP client + S3 presigned URLs**: don't use the default redirect
  follower — it carries `Authorization: Bearer` to the S3 host, which
  collides with the URL signature. Manual two-step (302 → Location →
  fresh request without auth) in `internal/studio/music/cache.go`.
- **modernc.org/sqlite multi-statement Exec**: works (good for ALTER
  TABLE blocks in migrations).

## Process for new work

When sergei asks for "next" / "continue" / "давай", consult the master
plan at `~/.claude/plans/skydive-memory-master-plan.md` (§3 phase order,
§4 dependency graph). Default: pick the next pending phase. State the
default + offer 1-2 alternatives + ask. If a phase is too big for one
session, do as much as is testable in one go and end with what wasn't
reached.

Each task is its own commit; sergei says when to commit. Use:

```bash
git -c user.name='pionerus' -c user.email='sergei.zamsharski@gmail.com' commit
```

because git config isn't set globally (per the system rules — don't edit
git config).
