DROP INDEX IF EXISTS idx_clients_assigned_operator;
ALTER TABLE clients DROP COLUMN IF EXISTS assigned_operator_id;
