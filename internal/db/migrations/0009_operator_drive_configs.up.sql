-- Per-operator Google Drive connection (Phase 9.4 / Drive integration).
--
-- Operator authenticates with Google via OAuth on /operator/storage; we
-- persist the refresh token (encrypted) here and use it to mint short-lived
-- access tokens whenever studio needs to upload a rendered artifact.
--
-- One row per operator (UNIQUE(operator_id)) — disconnecting deletes the
-- row, files already uploaded to Drive stay there (operator owns them).
-- tenant_id is denormalised so platform-admin queries don't have to join
-- through operators.
CREATE TABLE operator_drive_configs (
  id                       BIGSERIAL PRIMARY KEY,
  operator_id              BIGINT NOT NULL UNIQUE REFERENCES operators(id) ON DELETE CASCADE,
  tenant_id                BIGINT NOT NULL          REFERENCES tenants(id)   ON DELETE CASCADE,

  google_account_email     TEXT NOT NULL,           -- e.g. "ops@gmail.com" — shown in UI
  google_account_id        TEXT,                    -- subject claim from id_token (stable per Google account)

  refresh_token_enc        BYTEA NOT NULL,          -- AES-GCM(plaintext refresh_token, app key)
  access_token_cache       TEXT,                    -- short-lived bearer; refreshed lazily from refresh_token
  access_token_expires_at  TIMESTAMPTZ,             -- ~1h after each refresh

  root_folder_id           TEXT,                    -- "Skydive Memory" folder Drive id (created on first connect)
  scopes                   TEXT NOT NULL DEFAULT '',-- granted scopes joined by space; verified pre-upload

  last_health_check_at     TIMESTAMPTZ,
  last_health_check_ok     BOOLEAN,
  last_health_check_error  TEXT,

  created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_operator_drive_configs_tenant ON operator_drive_configs(tenant_id);

-- Per-jump folder cache. Each jump that lands on Drive gets one folder
-- "{client name} — {date}" inside the operator's root_folder_id; we cache
-- its Drive id here so re-rendering doesn't create a second sibling folder.
CREATE TABLE jump_drive_folders (
  jump_id           BIGINT  PRIMARY KEY REFERENCES jumps(id) ON DELETE CASCADE,
  operator_id       BIGINT  NOT NULL    REFERENCES operators(id) ON DELETE CASCADE,
  drive_folder_id   TEXT    NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_jump_drive_folders_operator ON jump_drive_folders(operator_id);
