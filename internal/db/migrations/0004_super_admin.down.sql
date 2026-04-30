-- Roll back super-admin tenant fields.
DROP INDEX IF EXISTS idx_tenants_status_plan;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS plan,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS country_code,
    DROP COLUMN IF EXISTS city;
