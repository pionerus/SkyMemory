-- =========================================================================
-- 0006_client_status.up.sql
--
-- Canonical 5-step lifecycle status per client (computed view, not stored).
--
--   new          → client row exists, no operator assigned yet
--   assigned     → operator assigned, no jump registered yet
--   in_progress  → jump exists (operator hit "Start project")
--   sent         → deliverables email went out to the client
--   downloaded   → client clicked the download button on /watch/<code>
--
-- Drives status pills on every list (admin/clients, operator/clients,
-- studio dashboard, platform/jumps). Computed from existing fields plus
-- one new column we add here: jumps.download_clicked_at.
-- =========================================================================

-- Track first-time download click on /watch page. NULL until the client
-- clicks Download for the main video (Phase 5 may add per-deliverable
-- timestamps; for now one "anything was downloaded" flag is enough).
ALTER TABLE jumps
    ADD COLUMN IF NOT EXISTS download_clicked_at TIMESTAMPTZ;

-- Canonical status per client. Joins each client to its most-recent jump
-- (LEFT JOIN LATERAL → exactly one row per client) and projects a single
-- text status using a CASE ladder. Latest jump wins; older jumps are
-- ignored for status purposes.
CREATE OR REPLACE VIEW v_client_status AS
SELECT
    c.id                            AS client_id,
    c.tenant_id,
    c.name,
    c.email,
    c.phone,
    c.access_code,
    c.assigned_operator_id,
    c.created_at                    AS client_created_at,
    latest.jump_id,
    latest.jump_created_at,
    latest.jump_status              AS internal_jump_status,
    latest.deliverables_email_sent_at,
    latest.download_clicked_at,
    CASE
        WHEN latest.download_clicked_at        IS NOT NULL THEN 'downloaded'
        WHEN latest.deliverables_email_sent_at IS NOT NULL THEN 'sent'
        WHEN latest.jump_id                    IS NOT NULL THEN 'in_progress'
        WHEN c.assigned_operator_id            IS NOT NULL THEN 'assigned'
        ELSE                                                    'new'
    END AS status
FROM clients c
LEFT JOIN LATERAL (
    SELECT
        j.id                         AS jump_id,
        j.created_at                 AS jump_created_at,
        j.status                     AS jump_status,
        j.deliverables_email_sent_at,
        j.download_clicked_at
    FROM jumps j
    WHERE j.client_id = c.id
    ORDER BY j.created_at DESC
    LIMIT 1
) latest ON true;
