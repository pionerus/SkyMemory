-- Migration 0002 — tenants watermark & intro/outro + jumps email tracking.
-- Foundation for Phase 6 (Branding) and Phase 13 (Email send post-render).
--
-- These columns are nullable / have safe defaults so existing rows keep working
-- without backfill. Pipeline reads them and applies overlay only when set.

ALTER TABLE tenants
    ADD COLUMN watermark_size_pct    INT  NOT NULL DEFAULT 12,    -- 5–25%, applied to width
    ADD COLUMN watermark_opacity_pct INT  NOT NULL DEFAULT 70,    -- 10–100%
    ADD COLUMN watermark_position    TEXT NOT NULL DEFAULT 'bottom-right',
    ADD COLUMN intro_clip_path       TEXT,                        -- S3 key, NULL = no intro
    ADD COLUMN outro_clip_path       TEXT;                        -- S3 key, NULL = no outro

ALTER TABLE tenants
    ADD CONSTRAINT tenants_watermark_position_check
    CHECK (watermark_position IN ('bottom-right','bottom-left','top-right','top-left'));

ALTER TABLE jumps
    ADD COLUMN deliverables_email_sent_at TIMESTAMPTZ,            -- when client got "your video is ready"
    ADD COLUMN deliverables_email_message TEXT;                   -- custom message from operator
