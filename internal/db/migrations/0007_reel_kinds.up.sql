-- =========================================================================
-- 0007_reel_kinds.up.sql
--
-- Extend jump_artifacts.kind enum so the studio can upload short-form
-- social deliverables alongside the main edit:
--
--   wow_highlights — pure freefall multi-cut, ~30-40s, music only.
--   (vertical is already there — used for the 9:16 Insta reel.)
--
-- Drop the old CHECK and re-add with the extended set. Existing rows
-- (horizontal_1080p / horizontal_4k / vertical / photo / screenshot) all
-- still validate.
-- =========================================================================

ALTER TABLE jump_artifacts
    DROP CONSTRAINT jump_artifacts_kind_check;

ALTER TABLE jump_artifacts
    ADD CONSTRAINT jump_artifacts_kind_check
    CHECK (kind IN (
        'horizontal_1080p',
        'horizontal_4k',
        'vertical',
        'wow_highlights',
        'photo',
        'screenshot'
    ));
