-- baki:compat expand
-- A policy's waivers and per-severity budgets (models.Policy Waivers, MaxErrors, MaxWarnings, MaxInfo). They were
-- never stored, so a policy saved in cloud mode lost them and evaluation by id ignored them. All nullable: NULL is
-- none (no waivers; no budget, which evaluation reads as 0 tolerated findings), and the rows written before read as
-- they always did. The release before neither inserts nor updates these columns, so a policy it saves keeps them.
ALTER TABLE policies
    -- The waivers as models.PolicyWaiver objects ({findingKey, reason, expiresAt}); NULL when there are none.
    ADD COLUMN IF NOT EXISTS waivers      JSONB,
    ADD COLUMN IF NOT EXISTS max_errors   BIGINT,
    ADD COLUMN IF NOT EXISTS max_warnings BIGINT,
    ADD COLUMN IF NOT EXISTS max_info     BIGINT;
