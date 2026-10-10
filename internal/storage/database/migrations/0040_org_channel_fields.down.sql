-- Reverses the organisation channel fields. Data-lossy by design: every channel's event subscription goes (each
-- receives the default set again), and the organisation jira and email channels, which cannot work without their
-- fields, are deleted.
DELETE FROM org_channels WHERE kind IN ('jira', 'email');
ALTER TABLE org_channels DROP COLUMN IF EXISTS events;
ALTER TABLE org_channels DROP COLUMN IF EXISTS secrets;
ALTER TABLE org_channels DROP COLUMN IF EXISTS config;
