-- ===========================================================================
-- 0005_assign_operator: club admin assigns each client to a specific operator.
--
-- Workflow:
--   1. Super admin creates a club.
--   2. Club admin (owner) adds operators within their club.
--   3. Club admin adds clients and picks which operator will film their jump.
--   4. The operator's portal shows clients where assigned_operator_id = them.
--
-- The column is nullable because (a) historical clients exist without an
-- assignment, (b) some clubs let operators pull from a shared pool. ON DELETE
-- SET NULL means removing an operator de-assigns their clients rather than
-- cascading the deletion.
-- ===========================================================================

ALTER TABLE clients
    ADD COLUMN IF NOT EXISTS assigned_operator_id BIGINT
        REFERENCES operators(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_clients_assigned_operator
    ON clients(assigned_operator_id)
    WHERE assigned_operator_id IS NOT NULL;
