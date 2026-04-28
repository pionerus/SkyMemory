# Freefall

Tandem-skydive video automation: operator drops 7 raw clips → system auto-edits with music ducking → delivers to client via personalised portal with code.

Two binaries from one repo:

- **`cmd/server`** — cloud HTTP service (multi-tenant admin, music library, client portal, Stripe + Montonio webhooks, Flowtark monthly billing).
- **`cmd/studio`** — local tool that runs on the operator's Windows machine. HTTP UI at `http://localhost:8080`, FFmpeg pipeline, S3-compatible upload to the club's storage. **Windows-only for now** (Mac support deferred).

Full architecture & domain model: `~/.claude/plans/https-freefall-ing-giggly-mist.md`.

## Local dev

Prerequisites: **Go 1.22+**, Docker Desktop (for Postgres + Redis + MinIO), FFmpeg on PATH.

### Easy mode (Windows cmd / double-click)

```
docker compose up -d
run-server.bat
run-studio.bat
```

`run-server.bat` and `run-studio.bat` auto-copy `.env.example` → `.env` on first run, build the binary if missing, and load env vars before launch. They work from `cmd.exe` (don't paste `# comments` into cmd — it doesn't strip them like PowerShell does).

### Manual mode

```powershell
# Load env (PowerShell)
Get-Content .env | Where-Object { $_ -match '^[A-Z_]+=' } | ForEach-Object {
    $name, $value = $_.Split('=', 2); [Environment]::SetEnvironmentVariable($name, $value, 'Process')
}
go run ./cmd/server     # port 8000
go run ./cmd/studio     # port 8080 (separate terminal)
```

- Studio UI: http://localhost:8080
- Cloud server: http://localhost:8000
- MinIO console: http://localhost:59001 (user: `freefall`, password: `freefall_dev_secret`)
- Postgres: `localhost:55433` (user/db: `freefall`, password: `freefall_dev`)
  - Note: 55432 is reserved for Flowtark on this machine, so Freefall uses 55433

## Build

```powershell
# Cloud server (Linux for prod)
go build -o bin/server.exe ./cmd/server

# Studio (Windows desktop tool)
go build -o bin/studio.exe ./cmd/studio
```

## Project layout

| Path | Role |
|---|---|
| `cmd/server` | Cloud HTTP entry |
| `cmd/studio` | Local Windows entry |
| `internal/api/v1` | Versioned DTO contract studio↔server |
| `internal/studio/...` | Pipeline + FFmpeg + state — used only by `cmd/studio` |
| `internal/billing` | Usage events + Flowtark + pluggable Stripe/Montonio |
| `internal/storage` | S3 client factory per-tenant |
| `migrations/` | golang-migrate SQL (`0001_init.up.sql` …) |
| `web/server/` | Cloud HTML templates + static |
| `build/windows/` | Installer scripts (deferred) |
| `deploy/cloud/` | Production deploy scripts |

## License

Proprietary. Source not for redistribution.
