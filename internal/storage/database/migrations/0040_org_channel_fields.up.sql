-- baki:compat expand
-- Organisation channel fields beyond url and secret, and per-channel event subscriptions (ADR 0021). Three nullable
-- or defaulted columns: the release before never names them, so its INSERT takes the defaults and its upsert's SET
-- list leaves them as they are, and it keeps reading and writing url and secret exactly as before.
--
-- config: the kind's public fields beyond url, a JSON object of strings (jira's email and project, an email
-- channel's recipients as comma-separated member ids). Organisation configuration, readable by members like the
-- rest of the row; an erased user's id is removed from recipients (lifecycle_orgs.go).
ALTER TABLE org_channels ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}'::jsonb;
-- secrets: the kind's secret fields beyond secret (jira's API token) as one JSON object, sealed with the field
-- encryption key ("enc:v1:", field_crypto.go) like url and secret, and re-sealed by a key rotation. '' is none.
ALTER TABLE org_channels ADD COLUMN IF NOT EXISTS secrets TEXT NOT NULL DEFAULT '';
-- events: the event types the channel receives, a JSON array of strings. NULL is the default set, the events every
-- organisation channel received before subscriptions (drift, health_regression, new_rules), so no existing channel
-- changes; the test event reaches every channel.
ALTER TABLE org_channels ADD COLUMN IF NOT EXISTS events JSONB;
