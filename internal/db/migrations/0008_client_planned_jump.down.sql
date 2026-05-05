-- Rollback: restore the v7 view shape and drop the new column.
DROP VIEW IF EXISTS v_client_status;
ALTER TABLE clients DROP COLUMN IF EXISTS planned_jump_at;

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
