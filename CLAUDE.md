# Freefall — project memory for Claude

> Read this first. Updated 2026-04-29.

## What this is

Multi-tenant SaaS for tandem-skydive operators. Camera operator films 7 canonical
segments (intro / interview_pre / walk / interview_plane / freefall / landing /
closing), studio auto-edits them with music + sidechain ducking, and delivers
the result to the jumper via a `/watch/<access_code>` portal with optional
paid photo pack.

**Two Go binaries from one repo:**

- **`cmd/server`** — cloud HTTP (will live on Hetzner Cloud). Auth, tenants,
  operators, license tokens, music library, jump records, Stripe/Montonio
  webhooks (later), Flowtark monthly billing (later).
- **`cmd/studio`** — Windows-native, runs on each operator's machine. HTTP UI
  on `localhost:8080`, drives the FFmpeg render pipeline. Talks to cloud over
  HTTPS using a Bearer license token.

Repo: https://github.com/pionerus/SkyMemory

Full design plan: `~/.claude/plans/https-freefall-ing-giggly-mist.md` (the
original brief from sergei). This CLAUDE.md is the **current state + roadmap**.

## Tech stack (locked in)

- **Go 1.26.2** (`C:\Program Files\Go\bin\go.exe` on the dev box; not in PATH
  for shell sessions, run with full path or via `run-*.bat` scripts)
- PostgreSQL 16 via `pgx/v5`, raw SQL, migrations via `golang-migrate` (cloud
  side; studio uses its own SQLite)
- Redis (cloud session store via `gorilla/sessions`)
- SQLite via `modernc.org/sqlite` (pure-Go, no cgo) for studio's local state
- FFmpeg + ffprobe — system binaries; in dev they're at
  `C:\Users\serge\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_…\bin\`
  (not in default PATH, studio finds them when launched from a fresh shell
  that has User PATH refreshed)
- `chi` HTTP router both sides
- `aws-sdk-go-v2` for S3 (works against Hetzner OS, MinIO, Backblaze B2)
- `bcrypt` for password hashing, SHA-256 hex for license token storage
- Caddy for production TLS (later)
- Sentry (later)

## Distribution stages

1. **Stage 1 (NOW)**: dev box only. `.\run-server.bat` + `.\run-studio.bat`
   from local PowerShell.
2. **Stage 2 (alpha)**: ship `studio.exe` unsigned to friendly operators —
   they click past Windows SmartScreen on first launch.
3. **Stage 3 (paying customers)**: Apple Dev ID ($99/yr) + Windows EV cert
   (~$300/yr).

**Mac is deferred** — owner has no Mac.

## Port map (dev — these are LIVE conflicts on this machine)

| Service | Host port | Notes |
|---|---|---|
| Cloud server | **8000** | server.exe |
| Studio | **8080** | studio.exe |
| Postgres | **55433** | NOT 55432 — Flowtark squats 55432 on this dev box |
| Redis | 56379 | (internal-only in prod) |
| MinIO S3 API | 59000 | swap to Hetzner Object Storage in prod |
| MinIO Console | 59001 | http://localhost:59001 user `freefall` pass `freefall_dev_secret` |

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

**Smoke check after restart:**

- http://localhost:8000/login — cloud root
- http://localhost:8000/admin/tokens — issue a license token (paste into
  `.env` STUDIO_LICENSE_TOKEN, restart studio)
- http://localhost:8000/admin/music-library — upload real MP3s (≠ random
  bytes — fake test files won't decode in browser audio preview)
- http://localhost:8080 — studio dashboard

## What's built (current state)

### Cloud (`cmd/server`)

- **Auth**: signup → login → /me → logout (cookie sessions, bcrypt cost 12,
  SameSite=Strict, Secure-in-prod). HTML pages at `/signup`, `/login`.
- **License tokens**: `POST/GET/DELETE /admin/license-tokens`. Plaintext shown
  ONCE on issue (SHA-256 stored). HTML page at `/admin/tokens`. Revoke is
  soft (audit row stays).
- **Music library**: admin upload via `/admin/music-library` HTML, S3-backed,
  per-track metadata (title, artist, license, duration, mood, suggested_for).
  Cloud-hosted MP3s served via 15-min presigned URLs.
- **Studio API** (Bearer license token):
  - `POST /api/v1/license/validate` — studio bootstrap
  - `POST /api/v1/jumps/register` — create client + jump, returns access_code
  - `GET  /api/v1/jumps/{id}` — status read
  - `PUT  /api/v1/jumps/{id}/music` — set/clear picked track
  - `GET  /api/v1/music` — catalog visible to tenant (NULL or own)
  - `POST /api/v1/music/suggest` — top-N scored picks
  - `GET  /api/v1/music/{id}/file` — 302 redirect to 30-min presigned URL
- **Migration 0001** (cloud Postgres): tenants, operators, license_tokens,
  clients, jumps, jump_artifacts, music_tracks, music_visible_to view,
  monthly_invoices, usage_events, tenant_payment_configs, tenant_storage_configs,
  photo_orders, processed_webhook_events, app_settings.

### Studio (`cmd/studio`)

- **Local SQLite** (`~/.freefall-studio/state.db`) — schema v5:
  - v1: projects (canonical fields)
  - v2: clips (per-segment metadata, ffprobe output, source_path under
    `~/.freefall-studio/jobs/<id>/`)
  - v3: clips trim columns (`trim_in_seconds`, `trim_out_seconds`,
    `trim_auto_suggested`)
  - v4: projects music columns (denormalised for offline read)
  - v5: generations (one row per Generate-button click; status, progress_pct,
    step_label, output_path, output_size, error)
- **License manager** — bootstrap call + 6h refresh, in-memory snapshot only
  (no offline grace yet). UI shows "License OK" / "License invalid (reason)".
- **5-step wizard, partially built:**
  - Step 0 (output choice): rendered into the New Project form (1080p / 4K /
    vertical / photos). Currently 4K + vertical + photos still **don't render**
    even when toggled — only 1080p comes out of the pipeline.
  - Step 1 (clip uploads): ✅ drag-drop or click-pick into 7 canonical slots
    + custom (`custom:label`) slots. ffprobe metadata stored per clip.
  - Step 2 (trim): ✅ inline `<video>` preview with range streaming + dual
    in/out sliders + Preview-window playback + Save. ✨ Auto-suggest button
    runs per-kind heuristic (silencedetect for audio kinds; positional for
    walk/freefall/landing). AI-suggested clips get a pill on the slot.
  - Step 2.5 (music): ✅ Browse library + ✨ Suggest panel scored by project
    duration + mood overlap. Picked track stored locally + pushed to cloud.
  - Step 3 (timeline reorder): ❌ not built. Clips render in canonical order,
    custom clips appended.
  - Step 4 (Generate): ✅ POST /generate kicks off goroutine, polled via
    /generations, output played inline + Download MP4 link.
- **FFmpeg pipeline (`internal/studio/pipeline/runner.go`):**
  - **Stage A** trim+normalise: per-clip → 1920×1080 30fps H.264 + AAC stereo
    48k. Action kinds (intro/walk/freefall/landing/closing/custom) get their
    audio replaced by `anullsrc` silence. Interview kinds keep speech.
  - **Stage B** crossfades: one ffmpeg with N inputs + filter_complex
    `xfade` (video) + `acrossfade` (audio), 0.5s default crossfade, shrunk
    to `clip_duration/3` for short clips. Single-clip projects skip.
    Returns the actual post-crossfade duration.
  - **Stage C** music mix (skipped if `music_track_id=0`):
    - Music looped, faded in 1s, pre-attenuated 0.7×.
    - Sidechain compressor — project audio drives the duck. Silence in
      action segments → no duck → music plays full. Speech in interview
      segments → ratio 8 duck → speech is clear.
    - Final result has 1s afade-out at the real concat-end timestamp.
  - **Stage D** stat → status='done' + output_path + size.
- **Music cache** (`~/.freefall-studio/music-cache/<id>.mp3`) — atomic
  download, manual 302-redirect handling (Go's default redirect carries
  Authorization cross-host and breaks S3 signature verification).

### Repo discipline

- Raw SQL only (no ORM). pgx uses `$1, $2…` placeholders.
- Multi-tenant scoping on every cloud query: `WHERE tenant_id = $1`.
- API contract lives in `internal/api/v1`; don't break field names/types
  within v1.
- Webhook handlers (later): idempotent via `processed_webhook_events
  (provider, event_id)` PK.
- Photo originals (later): UUID+salt in S3 key, presigned 15-min only after
  payment confirms.
- `tenants.plan` writes (later): only by Stripe webhook handler.
- All `Write`-ed Go files run `go vet ./...` clean.

### Memory aids

- Owner: sergei@youngearlystarters.com (Claude context),
  sergei.zamsharski@gmail.com (GitHub `pionerus`)
- Sister project: **Flowtark** (https://flowtark.com — invoice SaaS at
  `C:\Users\serge\OneDrive\Documents\Projects\invoice system`). Freefall
  sends monthly B2B bills via Flowtark's API (not yet wired).
- B2C photo pack default: €29 (`tenants.end_customer_photo_price_cents`)
- B2B per-jump default: €5 (`tenants.video_price_cents`)
- Access code format: 8 chars **Crockford Base32** (`0-9A-Z` minus `I/L/O/U`)
  → ~10¹² combos. Stored canonical (no dash) in DB; rendered as `XXXX-XXXX`.

### Common gotchas (paid for in tears already)

- **Stale processes**: `kill $PID` in bash often leaks server.exe / studio.exe
  zombies. Always check with `Get-Process -Name 'server','studio'` and
  `Get-NetTCPConnection -LocalPort 8000,8080,8002` before assuming code is
  broken.
- **bash on Windows + PATH**: `export PATH=` won't work with `C:/...` paths;
  use `/c/Users/...` form.
- **Python in inline test scripts**: `python` not `python3` on this machine.
- **Postgres 55432 vs 55433**: Flowtark's Postgres squats 55432 with a
  different `freefall_dev` password → auth failures look like "wrong code"
  but are actually the wrong DB. We use 55433.
- **MP3 fakes**: `head -c 12345 /dev/urandom > track.mp3` is a 12 KB random-
  byte file, NOT decodable. Browser audio preview silently fails. For real
  testing use `ffmpeg -f lavfi -i sine=...` or actual royalty-free downloads.
- **ffmpeg-on-PATH for studio**: studio's process inherits PATH from the
  shell that launched it. Updating PATH after install requires a fresh
  PowerShell window before `.\run-studio.bat`.
- **Go HTTP client + S3 presigned URLs**: don't use the default redirect
  follower — it carries `Authorization: Bearer` to the S3 host, which then
  collides with the URL signature. Manual two-step (302 → Location → fresh
  request without auth) in `internal/studio/music/cache.go`.
- **modernc.org/sqlite multi-statement Exec**: works (good for ALTER TABLE
  blocks in migrations).

## Roadmap — what's left

Numbered in approximate priority, but I usually just go top-to-bottom unless
asked otherwise.

### Pipeline polish (continues from current Generate)

1. **Final fade-in / fade-out on the whole video.** `acrossfade` already
   handles seams; we still need the very first frame to fade up from black
   and the very last to fade to black. ~3 lines in `mixMusic` /
   `concatWithCrossfades`.
2. **4K + vertical 1080×1920 outputs.** Currently the project's
   `output_4k` / `output_vertical` flags are stored but ignored. Add Stage
   B' / Stage B'' invocations that re-encode the same xfade chain at
   different resolutions. Vertical = `crop=1080:1920` from the centre of
   the 1080p result OR from the source if scale is wrong; tbd.
3. **Photos from freefall.** 15 keyframes from the freefall segment via
   `ffmpeg -ss N -frames:v 1`. If `has_operator_uploaded_photos=true`, skip
   auto-extract and just re-encode the operator's uploads through a watermark
   pass. Output goes to `~/.freefall-studio/jobs/<id>/photos/` and
   `photos.zip`.
4. **Operator's own DSLR photos upload.** New endpoint `POST
   /projects/{id}/photos` (multipart, multi-file). Studio UI: drop-zone in
   the Step 4 card.

### Cloud delivery

5. **Upload outputs to cloud storage.** Cloud bucket per tenant (or
   tenant-supplied via `tenant_storage_configs`). After Stage D, push
   `output_1080p.mp4`, optional 4K + vertical, and `photos.zip` to S3
   under `<tenant_id>/jumps/<jump_id>/`. Update `jump_artifacts` rows.
6. **`/watch/<access_code>` client portal.** Public page (no login). Streams
   the 1080p output via 24h presigned URL. Shows photo grid (low-res +
   watermarked until B2C payment). Cookie banner. Mobile-first design.
7. **Send-to-client email.** Operator's "Send" button → cloud emails the
   jumper a magic link. Resend (already used by Flowtark) over `flowfall.ing`
   domain.

### Billing

8. **Stripe + Montonio integration for B2C photo pack.** Pluggable
   `Provider` interface, idempotent webhook ledger. Tenants enable one or
   both.
9. **B2B usage tracking + Flowtark monthly cron.** Every `complete` →
   `usage_events('video_generated', amount_cents=tenant.video_price_cents)`.
   Cron 1st of month aggregates → POST to Flowtark API → email tenant owner.

### Studio-side polish

10. **Project list improvements**: archive with 90-day retention, soft
    delete with reload-from-disk warning.
11. **Edit & Regenerate flow**: open done project → modify → Regenerate
    rotates `access_code`, doesn't double-bill.
12. **Auto-suggest improvements**: GoCV motion-magnitude for walk/freefall
    (currently positional rules); face-detect for vertical centre-crop.
13. **System tray icon + auto-open browser** at studio start (UX polish).
14. **Bundle ffmpeg/ffprobe in studio.exe installer** so operator doesn't
    need a separate ffmpeg install.

### Production deploy

15. **Caddy + TLS on Hetzner Cloud**. Reuse Flowtark deploy.sh pattern.
16. **Sentry DSN wiring** (already imported, just needs prod values).
17. **Native Windows installer** (.msi via WiX or Inno Setup, unsigned for
    alpha).
18. **Apple Dev ID + .dmg notarisation** when Mac becomes a target.
19. **Production data backups** (pg_dump cron) + offsite copy.

### Out-of-band (don't forget)

- README.md hasn't been touched since scaffolding — refresh when shipping
  alpha.
- A delete/edit button on the music library admin page (currently can only
  upload, not remove).
- Domain registration: `freefall.ing` is the working brand; needs DNS once
  cloud is on real infra.

### Process for new work

When sergei asks for the next item, work top-to-bottom on this list unless
he says otherwise. Each task is its own commit + push to GitHub `main`.
Use `git -c user.name='pionerus' -c user.email='sergei.zamsharski@gmail.com' commit`
because git config isn't set globally (per the system rules — don't edit
git config).
