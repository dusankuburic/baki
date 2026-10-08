-- Reverses v36. Data-lossy by design: the waivers and budgets saved since are dropped; the policies stay.
ALTER TABLE policies
    DROP COLUMN IF EXISTS max_info,
    DROP COLUMN IF EXISTS max_warnings,
    DROP COLUMN IF EXISTS max_errors,
    DROP COLUMN IF EXISTS waivers;
