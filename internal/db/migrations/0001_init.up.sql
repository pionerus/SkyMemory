-- Freefall — initial schema (single migration file, mirror of plan §"Доменная модель")
-- All tenant-scoped tables index on tenant_id. Money is stored in cents (INT) with explicit currency column.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- =========================================================================
-- Tenants = парашютные клубы
-- =========================================================================
CREATE TABLE tenants (
    id BIGSERIAL PRIMARY KEY,
    slug TEXT UNIQUE NOT NULL,
    name TEXT NOT NULL,
    is_free_forever BOOLEAN NOT NULL DEFAULT false,

    -- B2B billing (что клуб платит нам, выставляется руками в админке)
    video_price_cents INT NOT NULL DEFAULT 500,        -- €5 / generated video
    photo_pack_price_cents INT NOT NULL DEFAULT 500,   -- €5 / sold photo pack
    billing_currency TEXT NOT NULL DEFAULT 'EUR',

    -- B2C end-customer photo price (что прыгун платит за фото-пак)
    end_customer_photo_price_cents INT NOT NULL DEFAULT 2900, -- €29

    -- Flowtark linkage for monthly invoice generation
    flowtark_client_id BIGINT,

    -- Photo watermark customization
    watermark_logo_path TEXT,                           -- optional per-tenant logo overlay

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

-- =========================================================================
-- Per-tenant storage configuration (where uploads land)
-- =========================================================================
CREATE TABLE tenant_storage_configs (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL UNIQUE REFERENCES tenants(id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('s3','minio','cloud_hosted')),
    endpoint_url TEXT,                                  -- e.g. https://nas.club.local:9000
    region TEXT,                                        -- 'auto' / 'eu-central-1'
    bucket TEXT NOT NULL,
    access_key_id TEXT NOT NULL,
    secret_access_key_enc BYTEA NOT NULL,               -- AES-GCM encrypted
    use_path_style BOOLEAN NOT NULL DEFAULT false,      -- true for MinIO

    last_health_check_at TIMESTAMPTZ,
    last_health_check_ok BOOLEAN NOT NULL DEFAULT false,
    last_health_check_error TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =========================================================================
-- Operators = users inside a tenant (camera operators + club admins)
-- =========================================================================
CREATE TABLE operators (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'operator' CHECK (role IN ('owner','operator')),
    email_verified_at TIMESTAMPTZ,
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(tenant_id, email)
);

-- =========================================================================
-- License tokens — used by studio.exe to talk to cloud server
-- =========================================================================
CREATE TABLE license_tokens (
    id BIGSERIAL PRIMARY KEY,
    operator_id BIGINT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    tenant_id BIGINT NOT NULL,                          -- denormalized for fast scope check
    token_hash TEXT NOT NULL UNIQUE,                    -- bcrypt(token)
    device_fingerprint TEXT,
    label TEXT,                                         -- 'Mac mini in editing booth'
    last_used_at TIMESTAMPTZ,
    last_used_ip INET,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX license_tokens_tenant ON license_tokens(tenant_id);

-- =========================================================================
-- Clients = the people who jumped
-- =========================================================================
CREATE TABLE clients (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    email TEXT,
    phone TEXT,
    access_code TEXT NOT NULL UNIQUE,                   -- 8 chars base32, no 0/O/1/I
    created_by BIGINT REFERENCES operators(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX clients_tenant ON clients(tenant_id);

-- =========================================================================
-- Jumps = one tandem jump = one video project
-- =========================================================================
CREATE TABLE jumps (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    client_id BIGINT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    operator_id BIGINT NOT NULL REFERENCES operators(id) ON DELETE RESTRICT,
    music_track_id BIGINT,                              -- FK added below after music_tracks exists

    status TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft','editing','encoding','uploading','ready','sent','delivered','failed')),

    -- Output toggles (Step 0 of wizard)
    output_1080p BOOLEAN NOT NULL DEFAULT true,
    output_4k BOOLEAN NOT NULL DEFAULT false,
    output_vertical BOOLEAN NOT NULL DEFAULT false,
    output_photos BOOLEAN NOT NULL DEFAULT false,

    -- Final output metadata
    duration_seconds INT,

    -- Photo pack state
    photo_pack_unlocked BOOLEAN NOT NULL DEFAULT false,
    photo_pack_price_cents_snapshot INT,                -- snapshot at jump-create time
    has_operator_uploaded_photos BOOLEAN NOT NULL DEFAULT false,

    -- Lifecycle timestamps
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jumps_tenant ON jumps(tenant_id);
CREATE INDEX jumps_client ON jumps(client_id);
CREATE INDEX jumps_operator ON jumps(operator_id);

-- =========================================================================
-- jump_artifacts — uploaded final outputs with ETag validation
-- =========================================================================
CREATE TABLE jump_artifacts (
    id BIGSERIAL PRIMARY KEY,
    jump_id BIGINT NOT NULL REFERENCES jumps(id) ON DELETE CASCADE,
    kind TEXT NOT NULL
        CHECK (kind IN ('horizontal_1080p','horizontal_4k','vertical','photo','screenshot')),
    variant TEXT
        CHECK (variant IS NULL OR variant IN ('preview','original')),
    s3_key TEXT NOT NULL,
    etag TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    width INT,
    height INT,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jump_artifacts_jump ON jump_artifacts(jump_id);

-- =========================================================================
-- Music library
-- =========================================================================
CREATE TABLE music_tracks (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT REFERENCES tenants(id) ON DELETE CASCADE,  -- NULL = global
    title TEXT NOT NULL,
    artist TEXT,
    license TEXT NOT NULL,                              -- 'CC0', 'CC-BY', 'YouTube Audio Library', etc
    s3_key TEXT NOT NULL,
    duration_seconds INT NOT NULL,
    bpm INT,
    mood TEXT[] NOT NULL DEFAULT '{}',                  -- {'epic','chill','fun'}
    suggested_for TEXT[] NOT NULL DEFAULT '{}',         -- {'intro','main','outro'}
    is_active BOOLEAN NOT NULL DEFAULT true,
    uploaded_by BIGINT REFERENCES operators(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX music_visibility ON music_tracks(tenant_id) WHERE is_active = true;

-- View enforcing the "only active" filter — every read goes through this view
CREATE VIEW music_visible_to AS
    SELECT * FROM music_tracks WHERE is_active = true;

-- Now that music_tracks exists, add the FK on jumps
ALTER TABLE jumps
    ADD CONSTRAINT jumps_music_track_id_fkey
    FOREIGN KEY (music_track_id) REFERENCES music_tracks(id) ON DELETE SET NULL;

-- =========================================================================
-- B2B billing — usage events feeding monthly invoices to clubs
-- =========================================================================
CREATE TABLE monthly_invoices (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    total_cents INT NOT NULL,
    currency TEXT NOT NULL,
    flowtark_invoice_id BIGINT,
    flowtark_invoice_number TEXT,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','sent','failed','paid')),
    generated_at TIMESTAMPTZ,
    failed_reason TEXT,
    UNIQUE(tenant_id, period_start)
);

CREATE TABLE usage_events (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    jump_id BIGINT REFERENCES jumps(id) ON DELETE SET NULL,
    event_type TEXT NOT NULL
        CHECK (event_type IN ('video_generated','photo_pack_sold','storage_hosted')),
    amount_cents INT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'EUR',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    monthly_invoice_id BIGINT REFERENCES monthly_invoices(id) ON DELETE SET NULL
);
CREATE INDEX usage_events_tenant_month ON usage_events(tenant_id, occurred_at);

-- =========================================================================
-- Pluggable B2C payment providers (Stripe + Montonio)
-- =========================================================================
CREATE TABLE tenant_payment_configs (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('stripe','montonio')),
    is_enabled BOOLEAN NOT NULL DEFAULT false,

    -- Stripe-specific
    stripe_publishable_key TEXT,
    stripe_secret_key_enc BYTEA,
    stripe_webhook_secret_enc BYTEA,
    stripe_account_id TEXT,                             -- for Stripe Connect

    -- Montonio-specific
    montonio_access_key TEXT,
    montonio_secret_key_enc BYTEA,
    montonio_environment TEXT CHECK (montonio_environment IS NULL OR montonio_environment IN ('sandbox','live')),

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(tenant_id, provider)
);

-- =========================================================================
-- Photo orders (B2C purchases via Stripe OR Montonio)
-- =========================================================================
CREATE TABLE photo_orders (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    jump_id BIGINT NOT NULL REFERENCES jumps(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('stripe','montonio')),

    -- Stripe fields
    stripe_session_id TEXT UNIQUE,
    stripe_payment_intent_id TEXT,

    -- Montonio fields
    montonio_order_uuid TEXT UNIQUE,
    montonio_payment_url TEXT,

    amount_cents INT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'EUR',
    status TEXT NOT NULL CHECK (status IN ('pending','paid','failed','refunded')),
    paid_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX photo_orders_jump ON photo_orders(jump_id);

-- =========================================================================
-- Webhook idempotency (works for both Stripe and Montonio)
-- =========================================================================
CREATE TABLE processed_webhook_events (
    provider TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);

-- =========================================================================
-- App settings — singleton row, mirrors Flowtark pattern
-- =========================================================================
CREATE TABLE app_settings (
    id INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- Stripe (platform-level keys for the cloud-hosted Stripe account, used as fallback if tenant doesn't bring own)
    stripe_mode TEXT NOT NULL DEFAULT 'test' CHECK (stripe_mode IN ('test','live')),
    stripe_publishable_key TEXT,
    stripe_secret_key TEXT,
    stripe_webhook_secret TEXT,

    -- Sentry
    sentry_dsn TEXT,

    -- Flowtark integration (so monthly cron can POST invoices)
    flowtark_api_url TEXT,
    flowtark_api_token TEXT,

    -- Encryption key for tenant_storage_configs.secret_access_key_enc + tenant_payment_configs.*_enc
    storage_secret_key BYTEA NOT NULL,

    -- TTLs for signed URLs
    signed_url_default_ttl_seconds INT NOT NULL DEFAULT 86400,
    paid_photo_url_ttl_seconds INT NOT NULL DEFAULT 900
);
INSERT INTO app_settings (id, storage_secret_key) VALUES (1, gen_random_bytes(32));
