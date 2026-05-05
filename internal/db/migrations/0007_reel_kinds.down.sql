ALTER TABLE jump_artifacts DROP CONSTRAINT jump_artifacts_kind_check;
ALTER TABLE jump_artifacts
    ADD CONSTRAINT jump_artifacts_kind_check
    CHECK (kind IN ('horizontal_1080p','horizontal_4k','vertical','photo','screenshot'));
