-- Reverse of 0001_init.up.sql. Drop in dependency-safe order.

DROP VIEW IF EXISTS music_visible_to;

DROP TABLE IF EXISTS app_settings;
DROP TABLE IF EXISTS processed_webhook_events;
DROP TABLE IF EXISTS photo_orders;
DROP TABLE IF EXISTS tenant_payment_configs;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS monthly_invoices;
DROP TABLE IF EXISTS music_tracks;
DROP TABLE IF EXISTS jump_artifacts;
DROP TABLE IF EXISTS jumps;
DROP TABLE IF EXISTS clients;
DROP TABLE IF EXISTS license_tokens;
DROP TABLE IF EXISTS operators;
DROP TABLE IF EXISTS tenant_storage_configs;
DROP TABLE IF EXISTS tenants;

-- pgcrypto extension is left in place — may be in use elsewhere.
