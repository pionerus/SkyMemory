# Skydive Memory — project memory for Claude

> Read this first. Updated 2026-05-03.
>
> **Canonical roadmap lives in `~/.claude/plans/skydive-memory-master-plan.md`.**
> When this file disagrees with the master plan, the master plan wins.
> Phase-state snapshots may also live in `~/.claude/projects/.../memory/`.

## What this is

Multi-tenant SaaS for tandem-skydive operators ("clubs"). Camera operators
film **5 canonical segments** (interview_pre / walk / interview_plane /
freefall / landing — intro/closing are now sourced from the club admin's
branding bundle, not operator-uploaded). The studio app auto-edits them
with music + sidechain ducking + watermark + intro/outro and delivers up
to 4 video deliverables + photo pack to the jumper via a `/watch/<access_code>`
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

## Schema state — Postgres v7 / Studio SQLite v7

**Cloud Postgres** at v7:

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
- **0006_client_status**: `jumps.download_clicked_at TIMESTAMPTZ` +
  view `v_client_status` projecting the canonical 5-step lifecycle
  per client: `new → assigned → in_progress → sent → downloaded`.
  Powers status pills across admin/clients, operator/clients,
  studio Today queue.
- **0007_reel_kinds**: extends `jump_artifacts.kind` CHECK to include
  `wow_highlights` (pure-freefall short reel). Existing kinds remain:
  `horizontal_1080p`, `horizontal_4k`, `vertical`, `wow_highlights`,
  `photo`, `screenshot`.

`jump_artifacts.kind` enum (post-0007): `horizontal_1080p`, `horizontal_4k`,
`vertical` (Insta 9:16 reel), `wow_highlights` (freefall-only highlight reel),
`photo`, `screenshot`. Pipeline now emits all 4 video kinds + photo pack
when the project has the corresponding output flags.

**Studio SQLite** at v7 (auto-migrated on Open at
`~/.freefall-studio/state.db`):

- v1 projects, v2 clips, v3 clip trim columns, v4 projects music columns,
  v5 generations, v6 `clip_cuts` (CUT / exclude feature),
  v7 `clips.speech_start_seconds` (post-action interview marker for
  hybrid silence/source audio in landing clips).

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

## What's built (current state — 2026-05-03)

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

### Phase 7 — cloud delivery + watch page (MOSTLY DONE)
- 7.1 DONE: studio uploads outputs to S3 after render. Two-step flow:
  `POST /api/v1/jumps/{id}/artifacts/upload-url` → presigned PUT →
  `POST /api/v1/jumps/{id}/artifacts` registers the row.
- 7.4 DONE: public `/watch/<access_code>` page (1:1 with WatchDesktop
  / WatchMobile design). 24h presigned video URL, watch link responsive.
- 7.6 DONE (Phase 5.5): photo grid section with 4-col → 2-col mobile,
  click → native browser image viewer. Reel + WOW download cards
  flip from "Coming soon" stubs to live cards when artifacts exist.
  Watch handler runs ONE `WHERE kind IN (...)` query for all
  artifacts, presigns each, caps photos at 20.
- 7.7 DONE: watch_events fire-and-forget logging on every page hit.
- 7.2 / 7.3 / 7.5 NOT YET: photo extraction from operator DSLR uploads
  (operator currently can only flag the slot; auto-extraction from
  freefall video already shipped via Phase 5 photo pack), deliverable
  picker overlay, watermarked-preview-vs-paid-original flow.

### Phase 5 — multi-deliverable pipeline + photo pack (DONE 2026-05-03)
- **Insta vertical reel** (`internal/studio/pipeline/runner_reel.go`):
  centre-cropped 9:16 multi-cut from walk + plane + freefall + landing.
  ~30 s, music-only audio, watermark on. Saved as `output_vertical.mp4`,
  uploaded as `kind='vertical'`.
- **WOW reel**: pure freefall 16:9, 4–6 sub-clips around exit + scene-
  anchored body cuts + canopy-open. Saved as `output_wow.mp4`,
  uploaded as `kind='wow_highlights'`.
- **Photo pack** (`internal/studio/highlights/photopack.go`): 20
  timestamps planned across walk/plane/freefall/canopy/landing with
  3-candidate sharpness picking (pure-Go Laplacian variance,
  `sharpness.go`). Falls back to operator's trim window for freefall
  body shots when RMS exit detection fails — always emits ≥16 photos
  on a typical clip set.
- **Highlights detection** (`internal/studio/highlights`):
  `FindExitMoment` + `FindCanopyOpenMoment` fuse `ffmpeg.AudioRMS`
  + `ffmpeg.SceneChanges` (scdet). Cascading fallback: detected
  anchors → operator trim window → whole clip.
- All three deliverables run in the SAME goroutine after the main
  1080p edit completes. Sequential (one QSV/CPU = no parallelism win).
  Total ~7–9 min for a full set including photos. Generate flags:
  `OutputVertical=true` triggers Insta + WOW; `OutputPhotos=true`
  triggers photo pack.

### Phase 4.x — auto-trim refresh (DONE 2026-05-03)
- **Auto-trim runs on upload** synchronously in `cmd/studio/main.go`
  upload handler — operator no longer has to click "Auto-trim".
  Page reload after upload picks up persisted trim values from DB.
  Operator opt-out via Settings modal: localStorage `studio.autoTrimOnUpload`
  → `X-Auto-Trim: 0` header on upload XHR → server skips heuristic.
- **Per-kind heuristics** (`internal/studio/trim/auto.go`):
  - Interview pre/plane: `silencedetect -30dB d=0.3` + 0.5 s pads,
    no max cap (was over-cutting).
  - Freefall: longest sustained-loud RMS window, adaptive threshold
    (median + 12 dB). Falls back to "keep full clip" when no window
    found (safer than the old skip-2/take-30 positional cut).
  - Landing: impact spike (RMS > –20 dB after ≥1 s of < –30 dB) →
    trim_in = spike – 1 s; smart trim_out via silencedetect on tail
    (last phrase + 1 s pad, fallback to clip end). **Auto-places
    speech-start marker** so pipeline ducks music under the
    post-landing interview without operator action.
- **`Suggestion.SpeechStart`** — new field on the auto-trim result.
  Currently only landing's smart heuristic sets it; manual marker
  via the Mark-speech-start button still works through its own
  endpoint.
- **Auto-save trim before Generate**: capture-phase JS click handler
  on `a[href*="/generate"]` walks all `.slot.has-file` rows, PUTs any
  whose live trim values differ from `data-trim-in`/`data-trim-out`,
  then navigates. Forgotten Save no longer loses dragged thumbs.

### Phase 5.5 — watch page surface for reels + photos (DONE 2026-05-03)
- `internal/jump/artifacts.go` `allowedArtifactKinds` extended with
  `wow_highlights` (mirrors migration 0007's CHECK).
- `internal/watch/handlers.go` `PageData` extended with
  `VerticalReelURL`, `WOWReelURL`, `Photos []Photo` — single
  `WHERE kind IN (...)` query, 24 h presign per artifact, photo
  cap = 20.
- `web/server/templates/watch.html` — "Coming soon" stubs for Reel
  and WOW now flip to live download cards via `{{if .VerticalReelURL}}`
  / `{{if .WOWReelURL}}`. New `.wt-photo-grid-section` between the
  pack CTA and share row, 4-col desktop → 2-col mobile.
- Lightbox V1 = `<a target="_blank">` opens presigned URL in new tab,
  native image viewer handles zoom/save. Full JS lightbox deferred.

### Phase 6 — branding + pipeline overlay (DONE)
- 6.1 + 6.2: `/admin/branding` real UI. Per-tenant `freefall-branding`
  bucket, watermark PNG + intro / outro mp4 upload + size / opacity /
  position config (`internal/branding`).
- 6.3 + 6.4: pipeline overlay watermark + intro / outro concat in one
  ffmpeg pass. Conditional pix_fmt and `-fps_mode cfr` for QSV stability.
- 6.5: studio `internal/studio/branding` cache with ETag invalidation.
- **Operator clip board no longer has intro/closing slots** — `KindIntro`
  and `KindClosing` are legacy constants (kept for backward compat),
  removed from `CanonicalKinds()`. Pipeline filters legacy intro/
  closing clips out of `ListClips` so old projects don't double-render
  them alongside the branding bumpers.

### Trim rail unified design (DONE 2026-05-03)
- Single 36 px-tall track with two yellow IN/OUT thumbs (was two
  separate `<input type=range>` rows). Cuts overlay the SAME track
  as red striped boxes — drag body to move, drag L/R handles to
  resize. Click empty area → new 1.5 s cut (POST `/cuts`).
- Speech-start marker as a green vertical line on the same track,
  draggable. Auto-placed by landing heuristic; manual via "Mark
  speech start" button. Persists via PUT `/clips/{kind}/speech-start`.
- Live cursor reflects video.currentTime during play / drag-scrub.
- New cloud endpoint `PUT /cuts/{id}` for resize (reuses existing
  POST/DELETE).

### 5-step lifecycle status (DONE 2026-05-03)
Migration 0006 added `v_client_status` view + `jumps.download_clicked_at`.
Status pills replace the old internal-jump-status mix everywhere:

| Status | When |
|---|---|
| `new` | Client row exists, no operator |
| `assigned` | Operator assigned, no jump yet |
| `in_progress` | Jump created (operator hit Start project) |
| `sent` | Deliverables email went out |
| `downloaded` | Client clicked Download on `/watch/<code>` |

Endpoint `POST /watch/{code}/download` stamps `download_clicked_at`
on first click via `sendBeacon`. Studio dashboard, admin/clients,
operator/clients all read `v_client_status.status`.

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
1. **Phase 5.6 follow-ups** (small):
   - Energy-aware music auto-pick for reels (don't reuse main-edit
     track). Needs `music_tracks.energy_score` column or RMS probe of
     the track itself.
   - Per-deliverable `watch_events` (currently all clicks go in
     `artifact_kind='horizontal_1080p'`). Needed for platform-admin
     analytics breakdown.
   - Photo dedupe `original` vs `preview` via `DISTINCT ON (s3_key)`
     when both variants exist.
   - Manual exit-mark fallback — operator click-to-mark exit moment
     when RMS detection fails (currently the trim-window fallback
     covers this passively).
2. **Phase 7 polish** — operator DSLR photo uploads (currently only
   the auto-extracted freefall stills work), watermarked previews
   for unpaid clients, deliverable picker overlay.
3. **Phase 8 — SEPA + photo pack purchase** (3 sessions). GoCardless
   vs Stripe SEPA research, IBAN mandate flow, photo-pack checkout.
4. **Phase 13 — email send post-render** (1-2 sessions). Resend
   integration, magic-link `/me/<token>` cross-jump access.

### Important for production
5. **Phase 9.x — operator portal storage**. `/operator/storage`
   (Drive/Dropbox/OneDrive OAuth + S3-compat config). Operator
   dashboard already real (KPI strip + Today queue + recent jumps).
6. **Phase 11 — club admin extensions**. Real dashboard "Recent jumps"
   query (currently returns 0). `/admin/storage` page. SEPA UI.
7. **Phase 12 — billing aggregation**. Cross-tenant MRR roll-up,
   monthly_invoices integration with Flowtark API. Photo-pack revenue
   split per club.

### Cleanup (Phase 14 backlog)
8. **Drop license_tokens** — migration dropping the table,
   removing `internal/studio/license` package, `/admin/license-tokens`
   routes, `/api/v1/license/validate` endpoint, `admin_tokens.html`
   template.
9. **Dashboard real queries** — club-admin `/` and platform-admin
   `/platform/` still show zeros for some stats. Wire to real
   aggregations (operator portal `/operator/` is now real).
10. **Phase 4.2 full MSE/WebCodecs preview** — current single-`<video>`
    sequencer works but has visible white-frame between clips. Real
    seamless preview needs MediaSource Extensions or WebCodecs.
11. **Photo lightbox** — V1 uses native browser image viewer via
    `target="_blank"`. Upgrade to in-page lightbox with zoom/swipe
    if user feedback demands.

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
  stereo 48k. Three audio modes per clip:
  - `modeSource` (interview kinds with audio): keep source via
    `writeClipAudioChainFromSource`.
  - `modeSilence` (action kinds, default): per-clip `anullsrc` lavfi
    of length `effDur`.
  - `modeHybrid` (action kind with operator-set `speech_start_seconds`,
    typical: landing): `writeClipAudioChainHybrid` splits each
    keep-segment at the marker — silence atrim'd from anullsrc before,
    source atrim'd from clip after — concat'd in the right order.
  CUT zones split into keep-segments and concatenated.
- **Stage B** crossfades: one ffmpeg with N inputs + filter_complex
  `xfade` (video) + `acrossfade` (audio), 0.5s default crossfade,
  shrunk to `clip_duration/3` for short clips.
- **Stage C** watermark + intro/outro: optional `[wm][mainV]overlay`
  with brand-defined position; intro/outro normalised through the same
  Stage A chain and concatenated. Watermark goes on main timeline ONLY
  (intro/outro stay clean — bumpers carry their own branding).
- **Stage D** music mix (skipped if `music_track_id=0`): looped, faded
  in 1s, pre-attenuated 0.7×; sidechain compressor with project audio
  as the duck driver (now also includes the hybrid landing tail).
- **Stage E** stat → `status='done'` + `output_path` + `output_size`.
- **Stage F** (post-render): upload `horizontal_1080p` to cloud +
  register `jump_artifacts` row + flip `jumps.status = ready`.
- **Stage G** (Phase 5 deliverables, `runner_reel.go` +
  `highlights.PlanPhotoPack` / `ExtractPhotoPack`): if
  `OutputVertical=true` → render Insta vertical reel + WOW reel via
  multi-input + xfade + watermark + music-only audio, upload as
  `vertical` and `wow_highlights`. If `OutputPhotos=true` → extract
  20 frames at picker timestamps with 3-candidate sharpness scoring,
  upload each as `kind='photo'`.

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
