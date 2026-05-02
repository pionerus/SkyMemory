# Skydive Memory — project memory for Claude

> Read this first. Updated 2026-05-02.
>
> **Canonical roadmap lives in `~/.claude/plans/skydive-memory-master-plan.md`.**
> When this file disagrees with the master plan, the master plan wins.
> Phase-state snapshots may also live in `~/.claude/projects/.../memory/`.

## What this is

Multi-tenant SaaS for tandem-skydive operators ("clubs"). Camera operators
film 7 canonical segments (intro / interview_pre / walk / interview_plane
/ freefall / landing / closing), the studio app auto-edits them with
music + sidechain ducking + watermark + intro/outro, and delivers up to
6 video deliverables + photos to the jumper via a `/watch/<access_code>`
portal.

Brand: **Skydive Memory** (was "Freefall" — repo + binaries kept the old
name; only user-facing strings rebranded). Domain target
`skydivememory.app`, slug pattern `<club>.skydivememory.app`. Brand color
`#FFC83D` (yellow), accent `#FF8A3D` (orange), squirrel-mask logo, Plus
Jakarta Sans. Light + dark themes (toggle persists in localStorage).

### Four portals (color-coded chip in every rail)

1. **Platform admin** 🟡 (`/platform/*`) — us / YES team. Cross-tenant
   ops, club CRUD, per-club pricing, cross-tenant operator + jump lists,
   billing aggregation (Phase 12). Auth: `platform_admins` table.
   Bootstrap via `bin/server.exe platform-admin add`.
2. **Club admin** 🧡 (`/admin/*`) — `operators` with `role='owner'`.
   Music library, branding, jump records, **adds operators**, **adds +
   assigns clients to operators**, settings.
3. **Operator portal (web)** 🔵 (`/operator/*`) — `operators` with
   `role='operator'`. Web dashboard showing **clients assigned to me**
   and **jumps I filmed**. Personal storage configs (Drive/Dropbox/
   OneDrive) — Phase 9.4 stub.
4. **Operator (studio.exe)** 🟣 — Windows-native render tool on the
   operator's machine. Same email + password as the web portal — see
   "Auth model" below.
5. **Client view** — public `/watch/<access_code>` + magic-link
   `/me/<token>` for cross-jump access (`client_email_links` table).
   Renders on `watch.html` (1:1 with WatchDesktop / WatchMobile from
   the v2 design pack).

`/auth/login` JS redirects by role: `owner → /`, `operator → /operator/`,
`platform → /platform/`. `auth.SessionData.PlatformAdminID` and
`OperatorID` are mutually exclusive — `Set` and `SetPlatformAdmin` clear
each other.

### Two Go binaries from one repo

- **`cmd/server`** — cloud HTTP. Repo + module path is still
  `github.com/pionerus/freefall`; don't refactor that.
- **`cmd/studio`** — Windows-native, runs on each operator's machine.
  HTTP UI on `localhost:8080`, drives the FFmpeg pipeline. Talks to
  cloud over HTTPS using **session cookies** (operator's email +
  password — see "Auth model"). Stays untouched as a desktop app —
  the operator web portal is *additional*, not a replacement.

Repo: https://github.com/pionerus/SkyMemory

## Tech stack

- **Go 1.26.2** (`C:\Program Files\Go\bin\go.exe` — not on PATH; use
  the full path or `run-*.bat`).
- PostgreSQL 16 via `pgx/v5`, raw SQL, migrations via `golang-migrate`
  (cloud). Studio uses its own SQLite via `modernc.org/sqlite` (pure-Go,
  no cgo).
- Redis (cloud session store via `gorilla/sessions`).
- FFmpeg + ffprobe — system binaries; in dev they're at
  `C:\Users\serge\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_…\bin\`
  (not on PATH; refresh User PATH in a fresh shell before
  `.\run-studio.bat`).
- `chi` HTTP router. `aws-sdk-go-v2` for S3 (Hetzner OS / MinIO / B2
  compatible). `bcrypt` (cost 12) for passwords; SHA-256 hex for legacy
  license-token storage.
- Caddy + TLS, Sentry — production deploy phase, not yet wired.

## Distribution stages

1. **Stage 1 (NOW)**: dev box only. Local PowerShell.
2. **Stage 2 (alpha)**: ship `studio.exe` unsigned to friendly operators
   — they click past Windows SmartScreen on first launch.
3. **Stage 3 (paying customers)**: Apple Dev ID ($99/yr) + Windows EV
   cert (~$300/yr).

**Mac is deferred** — owner has no Mac.

## Port map (dev)

| Service | Host port | Notes |
|---|---|---|
| Cloud server | **8000** | server.exe |
| Studio | **8080** | studio.exe |
| Postgres | **55433** | NOT 55432 — Flowtark squats 55432 with a different `freefall_dev` password. Auth failures look like "wrong creds" but are actually the wrong DB. |
| Redis | 56379 | internal-only in prod |
| MinIO S3 API | 59000 | swap to Hetzner Object Storage in prod |
| MinIO Console | 59001 | http://localhost:59001 — `freefall` / `freefall_dev_secret` |

S3 buckets:
- `freefall-music` — music library
- `freefall-branding` — per-tenant watermark PNG + intro/outro clips
- `freefall-deliverables` — rendered videos uploaded by studio.exe (Phase 7.1)

server.exe creates the buckets on boot (idempotent HeadBucket → CreateBucket).

## Schema state — Postgres v5 / Studio SQLite v6

**Cloud Postgres** at v5:

- **0001_init**: tenants (`video_price_cents`, `photo_pack_price_cents`,
  `end_customer_photo_price_cents`, `is_free_forever`, `flowtark_client_id`,
  `watermark_logo_path`), operators (role: owner|operator), license_tokens,
  clients (access_code Crockford-Base32), jumps, jump_artifacts, music_tracks,
  music_visible_to view, monthly_invoices, usage_events,
  tenant_payment_configs, tenant_storage_configs, photo_orders,
  processed_webhook_events, app_settings.
- **0002_tenants_branding**: `tenants` extended with `watermark_size_pct`,
  `watermark_opacity_pct`, `watermark_position`, `intro_clip_path`,
  `outro_clip_path`.
- **0003_4portal**: `platform_admins`, `operator_storage_configs`,
  `tenant_sepa_configs`, `client_email_links`, `watch_events`,
  `jump_terms`. `jumps` extended with `deliverables_email_sent_at`,
  `deliverables_email_message`.
- **0004_super_admin**: `tenants` extended with `plan` (starter/pro/enterprise,
  not currently used in UI), `status` (trial/active/overdue/archived),
  `country_code` (ISO-2), `city`. Index on (status, plan).
- **0005_assign_operator**: `clients` extended with `assigned_operator_id`
  (FK → operators ON DELETE SET NULL) + partial index. Powers the
  "club admin assigns client to operator" workflow.

`jump_artifacts.kind` enum supports up to 6 video deliverables per jump:
`video_main`, `video_instagram`, `video_wow_highlights`, `video_plane_exit`,
`video_freefall`, `video_operator_flyby`. Pipeline currently emits
`horizontal_1080p` only — multi-deliverable is Phase 5 in the master plan.

**Studio SQLite** at v6 (auto-migrated on Open at
`~/.freefall-studio/state.db`):

- v1 projects, v2 clips, v3 clip trim columns, v4 projects music columns,
  v5 generations, v6 `clip_cuts` (CUT / exclude feature).

## Auth model (UPDATED 2026-05-02 — license tokens deprecated)

**One source of truth: operator email + password.**

- Club admin creates an operator on `/admin/operators` (email +
  password). That account can sign in to:
  - `/login` (browser) → web `/operator/*` portal
  - `studio.exe` via `STUDIO_OPERATOR_EMAIL` + `STUDIO_OPERATOR_PASSWORD`
    in `.env`
- Studio at boot: POSTs `/auth/login` → server returns SessionData and
  sets a session cookie → studio's shared `*http.Client` (with
  `cookiejar`) carries it for every `/api/v1/*` call.
- Refresh: studio re-logs in every 6 hours; on a 401 from any
  downstream call we re-login on demand (`session.Manager.EnsureLogin`).

**License tokens are deprecated** but the table + endpoints still exist
for older studio binaries. New code does not issue or consume them.

- `/admin/tokens` and the "License tokens" rail item are hidden.
- `/api/v1/license/validate` endpoint stays for backward compat.
- Cleanup tasks (drop table / package / routes) parked for Phase 14.

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

**Demo accounts (created 2026-05-02, persisted in dev DB):**

| Role | URL | Email | Password |
|---|---|---|---|
| 🟡 Super admin | http://localhost:8000/platform/login | `ops@skydivememory.app` | `skydive-dev-2026` |
| 🧡 Club admin (owner) | http://localhost:8000/login | `owner@demo.test` | `club-admin-2026` |
| 🔵 Operator | http://localhost:8000/login (auto-redirects to `/operator/`) | `op@demo.test` | `operator-2026` |
| 🟣 Studio | (no login — uses operator's email + password from `.env`) | `STUDIO_OPERATOR_EMAIL` | `STUDIO_OPERATOR_PASSWORD` |

The 3 portals share one cookie — open in **separate browser profiles /
incognito** to view simultaneously.

**Smoke check after restart:**

- http://localhost:8000/login — login (redirects by role)
- http://localhost:8000/platform/clubs — super-admin clubs CRUD
- http://localhost:8000/admin/operators — club admin adds operators
- http://localhost:8000/admin/clients — club admin adds + assigns clients
- http://localhost:8000/operator/clients — operator's assigned clients
- http://localhost:8000/admin/branding — watermark + intro/outro upload
- http://localhost:8080 — studio dashboard

## Three-role workflow (end-to-end)

1. **Super admin** (us) creates a club via `/platform/clubs` → Add club
   modal. Fields: name, slug, country, city, owner email + password,
   per-video rate (€), per-photo-pack rate (€). Atomic transaction
   inserts `tenants` row + owner-role `operators` row.
2. **Club admin** (the owner) signs in to `/admin/*`. Adds operators
   via `/admin/operators` (each gets email + password). Adds clients
   via `/admin/clients` and picks an operator from the dropdown (or
   reassigns later by clicking the operator chip in the row).
3. **Operator** signs in to `/operator/*` (or starts `studio.exe` —
   same credentials). `/operator/clients` lists clients where
   `assigned_operator_id = self`. `/operator/projects` lists jumps
   where `operator_id = self`.
4. Operator opens **studio.exe**, picks a client, registers a jump
   (`POST /api/v1/jumps/register` via session cookie). Films + edits
   + renders.
5. Render done → studio uploads `horizontal_1080p` to S3 (per-tenant
   prefix in `freefall-deliverables` bucket) → `jumps.status = ready`.
6. The deliverable surfaces in:
   - `/operator/projects` of the camera operator
   - `/admin/clients` row's "latest jump" column for the club admin
   - `/platform/jumps` cross-tenant list for super admin
   - `/watch/<access_code>` for the jumper (24h presigned URL)

## What's built (current state — 2026-05-02)

### Phase 0 — baseline pipeline (DONE)
QSV-or-libx264 single-pass with xfade + acrossfade between clips,
sidechain ducking, fade-in/out, RunRegistry serialising encodes,
SmartScreen-friendly progress reporting via `out_time_us`.

### Phase 1 — schema + auth foundation (DONE)
Migrations 0002 + 0003 + 0004 + 0005 applied. 4-role auth (`owner` /
`operator` / platform-admin / unauthenticated). `RequirePlatformAdmin`,
`RequireOwner`, `RequireSession` middlewares. Login JS redirect by role.

### Phase 2 — wizard split (DONE)
`/projects/new` → `/projects/{id}/clips` → `/projects/{id}/generate`.
Old single page is now a 302 to `/clips`.

### Phase 3 — clip board + CUT (DONE)
- 3.1/3.2/3.4: project_detail rewrite. Sticky topbar with click-to-copy
  access code, 4-column always-open clip grid, mood-gradient music
  card.
- 3.3: `clip_cuts` table + paint-zone painter UI + pipeline
  split+concat (`writeClipVideoChain` / `writeClipAudioChainFromSource`).

### Phase 4 — timeline preview + walkthrough player (PARTIAL)
- 4.1 DONE: gradient flex-block strip with striped 45° crossfade bands,
  diamond playhead, sidechain-ducked waveform card (1:1 with v2 design
  pack).
- 4.2 PARTIAL: single-`<video>` JS state machine plays trim windows in
  sequence and skips cut zones. **Not** a true MSE / WebCodecs preview
  — that's a future polish.
- 4.3 DONE: existing Generate CTA + skip-preview path.

### Phase 6 — branding + pipeline overlay (DONE)
- 6.1 + 6.2: `/admin/branding` real UI. Per-tenant `freefall-branding`
  bucket, watermark PNG + intro / outro mp4 upload + size / opacity /
  position config (`internal/branding`).
- 6.3 + 6.4: pipeline overlay watermark + intro / outro concat in one
  ffmpeg pass. Conditional pix_fmt and `-fps_mode cfr` for QSV stability.
- 6.5: studio `internal/studio/branding` cache with ETag invalidation.

### Phase 7 — cloud delivery + watch page (PARTIAL)
- 7.1 DONE: studio uploads outputs to S3 after render. Two-step flow:
  `POST /api/v1/jumps/{id}/artifacts/upload-url` → presigned PUT →
  `POST /api/v1/jumps/{id}/artifacts` registers the row.
- 7.4 DONE: public `/watch/<access_code>` page (1:1 with WatchDesktop
  / WatchMobile design). 24h presigned video URL, watch link
  responsive, photo pack + Reel + WOW deliverable cards stubbed for
  later phases.
- 7.7 DONE: watch_events fire-and-forget logging on every page hit.
- 7.2 / 7.3 / 7.5 / 7.6 NOT YET: photo extraction, operator DSLR
  uploads, deliverable picker, photo grid + lightbox.

### Phase 10.2 — super-admin clubs CRUD (DONE)
- E1: `/platform/clubs` list with KPI strip + cross-tenant aggregations.
- E2: `/platform/clubs/{id}` detail with 12-month bar chart + recent
  jumps + owner contact panel.
- E3: Add-club modal — fields per real pricing model (per-video rate,
  per-photo-pack rate). Atomic insert tenant + owner.

### Phase 10.3 + 10.4 — super-admin operators + jumps (DONE)
Cross-tenant lists with KPI strip and rail consistency.

### Phase 10.5–10.7 — super-admin watch-links / billing / settings (STUB)
Unified rail across every `/platform/*` page. Each section shows a
"coming soon" empty card with phase tag + back-to-dashboard link, so
the rail is fully navigable.

### B6 — admin clients (DONE)
`/admin/clients` row table 1:1 with the v2 design. Add-client modal
with auto-generated 8-char Crockford access code + assigned-operator
dropdown. Click an operator chip in any row to reassign via prompt
picker. Search + filter chips.

### B7 — admin operators (DONE)
`/admin/operators` page (Phase 11.x) — adds operator-role and
extra-owner accounts (email + password + role). Client + jump counts
per operator. Owners can't self-delete. JSON sidecar at
`/admin/operators/json` powers the dropdowns elsewhere.

### Design system v2 (DONE)
- Light theme (`.sm.is-light` + `.sm-sky`) opt-in via toggle. The
  toggle button + `theme-toggle.js` are wired into every rail. Per-page
  default = dark.
- Watch page rebuilt to match WatchDesktop + WatchMobile (responsive).
- Music admin redesigned to YT-Studio row list with persistent player
  bar + 36-bar deterministic mini-waveforms + mood gradients.
- Role chips (`role-chip--super` / `--club` / `--operator` / `--studio`)
  in every rail header — instant portal identification.
- All super-admin templates share `.super-rail` (one CSS source) so
  the menu doesn't reshape between sections.

### Auth refactor — license tokens dropped (2026-05-02)
- New `internal/studio/session` package: cookie-jar-backed login.
- All four cloud-talking studio clients (`jump`, `music`, `branding`,
  `delivery`) use the shared `*http.Client` with cookie jar instead of
  bearer tokens.
- `/api/v1/*` cloud routes now go through `RequireSession` instead of
  `RequireLicenseToken`.
- License-token CRUD pages + `/api/v1/license/validate` endpoint stay
  alive for older studio binaries. Cleanup deferred to Phase 14.

### Studio diagnostics (2026-05-02)
- Persistent `~/.freefall-studio/studio.log` (append, no rotation).
- `GET /log?lines=N` endpoint + "Show studio log" pane on the generate
  screen + 3s auto-refresh while open + auto-open on failure.
- Panic-recovery wrapper around the pipeline goroutine: silent crashes
  now mark the generation row failed with the panic message instead
  of leaving it stuck in `trimming` forever.
- Stale-cleanup before each `/generate`: any in-progress row with no
  live registry slot is auto-failed so a new run can start.
- ffmpeg stderr piped through `log.Writer()` so studio.log captures
  `ffmpeg start` / `pid=N` / per-frame progress / `exit OK|error`.

### Pipeline QSV fix (2026-05-02)
- `format=nv12` step before encode (instead of letting auto-scaler
  do it).
- `-fps_mode cfr` to handle mixed-rate inputs (50fps freefall +
  29.97fps intro/closing).
- Dropped `-low_power 1` — VDENC fast-path was crashing with
  "Invalid FrameType:0" on this dev box's UHD 620.
- **Auto-fallback to libx264** if QSV still fails: pipeline retries
  the same filter graph with libx264 inline and disables QSV for the
  rest of the studio session.

## What's left (priority order)

### Critical for alpha
1. **Phase 5 — multi-deliverable pipeline** (4-5 sessions). Currently
   only `horizontal_1080p` is produced. Need `vertical_1080`, `instagram_reel`,
   `wow_highlights`, `freefall_only`, `plane_exit`, `operator_flyby`.
2. **Phase 7 polish** — photo extraction (15 freefall keyframes via
   `ffmpeg -ss N -frames:v 1`), operator DSLR uploads, photo-grid
   lightbox on watch page, watermarked previews for unpaid clients.
3. **Phase 8 — SEPA + photo pack purchase** (3 sessions). GoCardless
   vs Stripe SEPA research, IBAN mandate flow, photo-pack checkout.
4. **Phase 13 — email send post-render** (1-2 sessions). Resend
   integration, magic-link `/me/<token>` cross-jump access.

### Important for production
5. **Phase 9.x — operator portal real impl**. `/operator/dashboard`,
   `/operator/storage` (Drive/Dropbox/OneDrive OAuth + S3-compat config).
6. **Phase 11 — club admin extensions**. Real dashboard "Recent jumps"
   query (currently returns 0). `/admin/storage` page. SEPA UI.
7. **Phase 12 — billing aggregation**. Cross-tenant MRR roll-up,
   monthly_invoices integration with Flowtark API. Photo-pack revenue
   split per club.

### Cleanup (Phase 14 backlog)
8. **Drop license_tokens** — migration 0006 dropping the table,
   removing `internal/studio/license` package, `/admin/license-tokens`
   routes, `/api/v1/license/validate` endpoint, `admin_tokens.html`
   template.
9. **Dashboard real queries** — club-admin `/` and platform-admin
   `/platform/` currently show zeros for stats. Wire to real
   aggregations.
10. **Phase 4.2 full MSE/WebCodecs preview** — current single-`<video>`
    sequencer works but has visible white-frame between clips. Real
    seamless preview needs MediaSource Extensions or WebCodecs.

### Production deploy
11. **Phase 14 — production deploy**. Caddy + TLS on Hetzner Cloud.
    Sentry DSN wiring (already imported, needs prod values). pg_dump
    cron + offsite copy.
12. **Phase 15 — distribution + signing**. Apple Dev ID + Windows EV
    cert when paying customers come on board.

### Sub-bugs / nice-to-have (small)
- Tip: `bin/studio.exe` needs `STUDIO_OPERATOR_EMAIL` set; if blank,
  pipeline is disabled (logged as `credentials_missing`).
- Studio log file has no rotation (append forever). Phase 16.
- Add-client form has no operator-dropdown UX upgrade — currently a
  `<select>` and a `prompt()` for reassignment. A real dropdown with
  search lands in Phase 11.
- Studio's "Light theme" doesn't follow web's choice (separate
  localStorage key in different domain). Could share via OS-prefs.

## Studio FFmpeg pipeline (`internal/studio/pipeline/runner.go`)

Single-pass per generation. The render produces the main timeline with
optional intro / outro concat and watermark overlay in one ffmpeg call:

- **Stage A** trim+normalise: per-clip → 1920×1080 30fps H.264 + AAC
  stereo 48k. Action kinds (intro/walk/freefall/landing/closing/custom)
  get their audio replaced by `anullsrc` silence. Interview kinds keep
  speech. CUT zones split into segments and concatenated.
- **Stage B** crossfades: one ffmpeg with N inputs + filter_complex
  `xfade` (video) + `acrossfade` (audio), 0.5s default crossfade,
  shrunk to `clip_duration/3` for short clips.
- **Stage C** watermark + intro/outro: optional `[wm][mainV]overlay`
  with brand-defined position; intro/outro normalised through the same
  Stage A chain and concatenated.
- **Stage D** music mix (skipped if `music_track_id=0`): looped, faded
  in 1s, pre-attenuated 0.7×; sidechain compressor with project audio
  as the duck driver; 1s afade-out.
- **Stage E** stat → `status='done'` + `output_path` + `output_size`.
- **Stage F** (post-render): upload to cloud + register
  `jump_artifacts` row + flip `jumps.status = ready`.

QSV reality on this dev box: Intel UHD 620 iGPU. `h264_qsv` with
`-low_power 1` crashed with "Invalid FrameType:0" — disabled. Default
PAK encode runs at ~1.5× realtime for 5min outputs. **Auto-falls back
to libx264 inline** if QSV still fails. Real-world cost on UHD 620:
~2× realtime CPU encode. Acceptable.

RunRegistry serialises generations to avoid QSV deadlock from parallel
encodes on the single iGPU engine.

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
- **Sessions and platform sessions are mutually exclusive** in one
  cookie. To test multi-role workflows, use separate browser profiles
  / incognito windows.

## Memory aids

- Owner: sergei@youngearlystarters.com (Claude context),
  sergei.zamsharski@gmail.com (GitHub `pionerus`).
- Sister project: **Flowtark** (https://flowtark.com — invoice SaaS at
  `C:\Users\serge\OneDrive\Documents\Projects\invoice system`).
  Skydive Memory will send monthly B2B bills via Flowtark's API
  (Phase 12, not yet wired).
- **Pricing model** (per-jump rates, NOT plan tiers):
  - B2B (us → club): `tenants.video_price_cents` (default €5,
    charged per delivered video) + `tenants.photo_pack_price_cents`
    (default €5, charged per photo pack sold). Set on Add-club form.
  - B2C (club → jumper): `tenants.end_customer_photo_price_cents`
    (default €29). Photo pack price the jumper pays.
- Access code format: 8 chars **Crockford Base32** (`0-9A-Z` minus
  `I/L/O/U`) → ~10¹² combos. Stored canonical (no dash) in DB; rendered
  as `XXXX-XXXX`.
- Brand assets: `internal/studio/ui/static/squirrel-mark.png` and
  `web/server/templates/static/squirrel-mark.png` — used via CSS
  `mask-image` (`.sm-mark` class). Inside `.sm-tile` the mark colour
  is hardcoded `#0B0E14` (dark navy on yellow tile) to stay visible
  in light theme too.
- Token list in `internal/studio/ui/static/skydive-memory.css` (mirrored
  in `web/server/templates/static/`).
- `theme-toggle.js` shared between both apps. Persistence via
  localStorage `sm-theme`.
- Old palette (indigo/peach/lavender pastels) is gone except buried
  inline `<style>` in `project_detail.html` overridden via dark-theme
  override layer at the end of that block.

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
- **bash `&` subshell scope**: when you start two background processes
  in one line, env vars set before the first `&` don't propagate to
  processes after the second `&`. Use `nohup env VAR=val ./bin` or wrap
  the whole thing in `(...)`.
- **Working dir** sometimes is `…\Freefall video` and sometimes
  `…\Freefall video\build`. Bash tool resets cwd between calls — either
  prepend `cd "/c/Users/serge/OneDrive/Documents/Projects/Freefall video"`
  or use absolute paths.
- **Python in inline test scripts**: `python` not `python3` on this
  machine.
- **Postgres 55432 vs 55433**: see port-map table — Flowtark squats
  55432.
- **MP3 fakes**: random-byte files don't decode; use
  `ffmpeg -f lavfi -i sine=…` or actual royalty-free downloads.
- **ffmpeg-on-PATH for studio**: studio's process inherits PATH from
  the shell that launched it. Updating User PATH after install requires
  a fresh PowerShell window before `.\run-studio.bat`.
- **Go HTTP client + S3 presigned URLs**: don't use the default redirect
  follower — it carries cookies/auth to the S3 host, which collides with
  the URL signature. Manual two-step (302 → Location → fresh request
  without auth) in `internal/studio/music/cache.go`.
- **modernc.org/sqlite multi-statement Exec**: works (good for ALTER
  TABLE blocks in migrations).
- **QSV on UHD 620**: `-low_power 1` crashes with "Invalid FrameType:0"
  on mixed-rate inputs. Pipeline drops it + auto-falls back to libx264.

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
