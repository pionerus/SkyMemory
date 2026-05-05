-- Planned jump date — set by club admin when adding a client. Distinct
-- from jumps.created_at (the actual jump record, written when the studio
-- registers a session). Lets the admin scheduling app show "AT 2026-05-12"
-- in the row before the jumper has even shown up at the dropzone.
--
-- Also surfaces it on the v_client_status view so /admin/clients gets it
-- in one query alongside the latest-jump status.
ALTER TABLE clients
  ADD COLUMN planned_jump_at TIMESTAMPTZ;

DROP VIEW IF EXISTS v_client_status;
CREATE VIEW v_client_status AS
SELECT
  c.id                AS client_id,
  c.tenant_id,
  c.name,
  c.email,
  c.phone,
  c.access_code,
  c.created_at        AS client_created_at,
  c.assigned_operator_id,
  c.planned_jump_at,
  j.id                AS jump_id,
  j.created_at        AS jump_created_at,
  j.deliverables_email_sent_at,
  j.download_clicked_at,
  CASE
    WHEN j.id IS NULL AND c.assigned_operator_id IS NULL                       THEN 'new'
    WHEN j.id IS NULL                                                          THEN 'assigned'
    WHEN j.download_clicked_at IS NOT NULL                                     THEN 'downloaded'
    WHEN j.deliverables_email_sent_at IS NOT NULL                              THEN 'sent'
    ELSE 'in_progress'
  END                 AS status
FROM clients c
LEFT JOIN LATERAL (
  SELECT id, created_at, deliverables_email_sent_at, download_clicked_at
  FROM jumps
  WHERE client_id = c.id
  ORDER BY created_at DESC
  LIMIT 1
) j ON TRUE;
