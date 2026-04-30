-- Reverse of 0002.

ALTER TABLE jumps
    DROP COLUMN IF EXISTS deliverables_email_sent_at,
    DROP COLUMN IF EXISTS deliverables_email_message;

ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_watermark_position_check;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS watermark_size_pct,
    DROP COLUMN IF EXISTS watermark_opacity_pct,
    DROP COLUMN IF EXISTS watermark_position,
    DROP COLUMN IF EXISTS intro_clip_path,
    DROP COLUMN IF EXISTS outro_clip_path;
