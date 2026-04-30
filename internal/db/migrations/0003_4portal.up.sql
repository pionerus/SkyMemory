-- Migration 0003 — four-portal architecture foundation.
-- Adds:
--   • platform_admins        (us — YES — outside any tenant)
--   • operator_storage_configs (per-operator personal storage: gdrive/dropbox/onedrive/s3)
--   • tenant_sepa_configs    (B2C SEPA direct debit for photo packs)
--   • client_email_links     (magic-link auth for jumpers)
--   • watch_events           (analytics for /watch/<code> clicks)
--   • jump_terms             (per-jump waiver/notes)
--
-- All independent — can ship without UI yet. Endpoints + portals follow in
-- later phases per master plan.

-- =========================================================================
-- Platform admins — single tier above all tenants. Handles cross-tenant
-- ops, pricing for clubs, billing aggregation. Bootstrapped via
-- `server.exe platform-admin add <email>`.
-- =========================================================================
CREATE TABLE platform_admins (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    name          TEXT NOT NULL,
    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE INDEX idx_platform_admins_email_active
    ON platform_admins(email) WHERE deleted_at IS NULL;

-- =========================================================================
-- Per-operator personal storage. Sits ALONGSIDE tenant_storage_configs;
-- studio render falls back operator → tenant → cloud-hosted.
--
-- For OAuth providers (gdrive/dropbox/onedrive) we store refresh tokens
-- AES-GCM encrypted via the same storage_secret_key as tenant configs.
-- For raw S3-compatible we use the same fields as tenant_storage_configs.
-- =========================================================================
CREATE TABLE operator_storage_configs (
    id          BIGSERIAL PRIMARY KEY,
    operator_id BIGINT NOT NULL UNIQUE REFERENCES operators(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL CHECK (provider IN ('gdrive','dropbox','onedrive','s3')),

    -- OAuth (gdrive/dropbox/onedrive)
    oauth_refresh_token_enc BYTEA,
    oauth_access_token_enc  BYTEA,
    oauth_expires_at        TIMESTAMPTZ,
    oauth_account_email     TEXT,                       -- "Connected to: alice@gmail.com"

    -- S3-compatible (only when provider='s3')
    endpoint_url            TEXT,
    region                  TEXT,
    bucket                  TEXT,
    access_key_id           TEXT,
    secret_access_key_enc   BYTEA,
    use_path_style          BOOLEAN NOT NULL DEFAULT false,

    -- Health probe — last attempt to PUT a 1-byte test object
    last_health_check_at    TIMESTAMPTZ,
    last_health_check_ok    BOOLEAN NOT NULL DEFAULT false,
    last_health_check_error TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =========================================================================
-- SEPA direct debit reqs for clubs. B2C only (jumper pays club for photos).
-- B2B (club pays us) goes through Flowtark → bank, not via this table.
-- =========================================================================
CREATE TABLE tenant_sepa_configs (
    tenant_id           BIGINT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    iban                TEXT NOT NULL,
    bic                 TEXT,                            -- optional in EU
    account_holder_name TEXT NOT NULL,
    creditor_id         TEXT,                            -- e.g. German Gläubiger-ID DE98...
    is_enabled          BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =========================================================================
-- Magic-link auth for clients (jumpers). The first email after a render
-- contains the unauthenticated /watch/<access_code> URL; if the jumper
-- wants to view ALL their jumps (multiple jumps under same email), they
-- request a magic link → token row → /me/<token> verifies and starts a
-- client-scoped session.
-- =========================================================================
CREATE TABLE client_email_links (
    id         BIGSERIAL PRIMARY KEY,
    client_id  BIGINT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,                     -- sha256(token), token never stored plaintext
    expires_at TIMESTAMPTZ NOT NULL,                     -- 30 minutes default
    used_at    TIMESTAMPTZ,                              -- one-shot
    ip         INET,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_client_email_links_unused_active
    ON client_email_links(client_id, expires_at) WHERE used_at IS NULL;

-- =========================================================================
-- Watch event analytics. Used by platform admin dashboard ("watch link
-- clicks +34%") and by operator/club to see which jumps got opened.
-- Insert on every page load of /watch/<code> (deduped per session).
-- =========================================================================
CREATE TABLE watch_events (
    id            BIGSERIAL PRIMARY KEY,
    jump_id       BIGINT NOT NULL REFERENCES jumps(id) ON DELETE CASCADE,
    artifact_kind TEXT,                                  -- which deliverable was opened
    referrer      TEXT,
    user_agent    TEXT,
    ip            INET,
    session_hash  TEXT,                                  -- dedupe within session
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_watch_events_jump_time ON watch_events(jump_id, created_at DESC);
CREATE INDEX idx_watch_events_recent ON watch_events(created_at DESC);

-- =========================================================================
-- Per-jump waiver / terms / operator notes. Mostly free-form for now —
-- club admin UI in Phase 11 lets owner add a note + capture signature.
-- =========================================================================
CREATE TABLE jump_terms (
    jump_id               BIGINT PRIMARY KEY REFERENCES jumps(id) ON DELETE CASCADE,
    waiver_signed_at      TIMESTAMPTZ,
    waiver_signature_name TEXT,                          -- typed-name signature
    notes                 TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
