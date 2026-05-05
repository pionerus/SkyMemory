# Google Drive integration — Google Cloud project setup

One-time manual setup that has to happen before operators can connect their
Drive on `/operator/storage`. After completion, drop the credentials into
`.env` (see step 4) and restart the cloud server.

---

## 1. Create / pick a Google Cloud project

1. Go to https://console.cloud.google.com.
2. Top-left **project selector** → **New project**.
3. Name: `Skydive Memory`. Organisation: leave default. Click **Create**.
4. Wait ~30s for the project to provision; switch to it.

## 2. Enable the Drive API

1. Navigation menu → **APIs & Services** → **Library**.
2. Search "Google Drive API". Click → **Enable**.
3. Repeat for "Google People API" (used by `userinfo.email`).

## 3. OAuth consent screen

1. **APIs & Services** → **OAuth consent screen**.
2. **User type** → choose **External** (so non-Google-Workspace operators
   can connect). Click **Create**.
3. App information:
   - App name: `Skydive Memory`
   - User support email: your email
   - App logo: optional (squirrel-mark.png from `internal/studio/ui/static/`)
   - App domain: `https://skydivememory.app` (placeholder until domain lives)
   - Authorized domains: `skydivememory.app`
   - Developer contact: your email
   - Click **Save and continue**.
4. **Scopes** → **Add or remove scopes**:
   - `…/auth/drive.file` — Per-file access to files created or opened by the app
   - `…/auth/userinfo.email`
   - `openid`
   - Click **Update** → **Save and continue**.
5. **Test users** → **Add users**: add the gmail address(es) you'll test
   with (your own + a couple of friendly operators). **Up to 100** test
   users allowed before verification.
6. **Summary** → **Back to dashboard**.

> **Production note**: while in *Testing* mode only test users can connect.
> For public release, click **Publish app** → submit for verification
> (4-6 weeks; need privacy policy URL + verified domain ownership).
> Stage 2 (alpha distribution per CLAUDE.md) works fine in Testing mode.

## 4. OAuth client ID

1. **APIs & Services** → **Credentials** → **+ Create credentials** →
   **OAuth client ID**.
2. **Application type**: Web application.
3. Name: `Skydive Memory cloud`.
4. **Authorized redirect URIs** → add:
   - `http://localhost:8000/auth/google-drive/callback` (dev)
   - `https://api.skydivememory.app/auth/google-drive/callback` (prod, when domain is live)
5. Click **Create**.
6. Modal pops up with **Client ID** + **Client secret**. Copy both.

## 5. Drop into `.env`

Edit the cloud server's `.env`:

```dotenv
GOOGLE_OAUTH_CLIENT_ID=123456789012-abcdef.apps.googleusercontent.com
GOOGLE_OAUTH_CLIENT_SECRET=GOCSPX-...
# Optional — defaults to ${FREEFALL_PUBLIC_BASE_URL}/auth/google-drive/callback
# GOOGLE_OAUTH_REDIRECT_URL=http://localhost:8000/auth/google-drive/callback
```

Restart:

```powershell
.\run-server.bat
```

## 6. Smoke test

1. Sign in as `op@demo.test` at http://localhost:8000/login (or any test
   user added in step 3.5; that gmail must be the one accepting the consent
   screen).
2. Open http://localhost:8000/operator/storage. Banner should be gone.
3. Click **Connect Google Drive** → consent screen → tick `drive.file`
   permission → confirm.
4. Land back on `/operator/storage` with green flash "connected".
5. Click **Test connection** → should print "Drive responding · folder ok".
6. Open https://drive.google.com in a new tab → root → see `Skydive Memory`
   folder ✓.
7. Click **Disconnect** → confirm → row deleted, future renders fall back
   to S3.

---

## What's NOT in Phase A/B yet

- **Renders go to S3, not Drive** — Phase C wires the upload. Phase A/B
  only proves we can connect, ensure folder, list, revoke. That's the
  scaffolding.
- **Watch page download button** — still S3-presigned. Drive URL rendering
  ships in Phase D.
- **Per-jump folder creation** — the planner calls `EnsureJumpFolder`
  starting Phase C.

## Troubleshooting

| Symptom | Likely fix |
|---|---|
| `error=not_configured` after clicking Connect | `.env` not loaded — restart server, verify `GOOGLE_OAUTH_CLIENT_ID` is set |
| Redirect URI mismatch on Google's consent page | Add the exact URL (incl. port + scheme) to step 4.4 |
| `error=invalid_grant` on callback | Test user not added to the consent screen, or `prompt=consent` was lost (re-attempt) |
| `Drive did not return a refresh_token` | Operator already granted previously; revoke at https://myaccount.google.com/permissions then reconnect |
| Test connection 502 with `quota exceeded` | Drive Free has 1k req/100s — wait 100s. Won't happen in normal use |
