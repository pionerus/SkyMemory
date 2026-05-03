-- ===========================================================================
-- 0004_super_admin: tenant plan / status / country fields powering the
-- super-admin (platform) /platform/clubs portal — Phase 10.2.
--
-- Adds presentation/billing-state fields to `tenants` so the platform-admin
-- clubs list can show plan badges (Starter / Pro / Enterprise), status pills
-- (trial / active / overdue / archived), and country flags + city. The
-- billing semantics behind `plan` get hooked up by Phase 12 (Stripe); for
-- now this is just metadata.
--
-- Defaults:
--   plan        = 'starter'   — every freshly-signed-up tenant is on Starter
--   status      = 'active'    — match existing rows; new sign-ups go through
--                                a Phase-12 trial check that may flip them
--                                back to 'trial'.
-- ===========================================================================

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS plan TEXT NOT NULL DEFAULT 'starter'
        CHECK (plan IN ('starter','pro','enterprise')),
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('trial','active','overdue','archived')),
    ADD COLUMN IF NOT EXISTS country_code TEXT
        CHECK (country_code IS NULL OR (length(country_code) = 2 AND country_code = upper(country_code))),
    ADD COLUMN IF NOT EXISTS city TEXT;

-- Index on (status, plan) lets the platform clubs list filter+sort cheaply
-- when the cohort grows.
CREATE INDEX IF NOT EXISTS idx_tenants_status_plan ON tenants(status, plan);
