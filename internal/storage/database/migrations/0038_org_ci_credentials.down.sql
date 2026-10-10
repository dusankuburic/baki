-- Reverses v38. Data-lossy by design: every organisation CI credential is dropped, and runners signing with one get
-- 401 until an admin mints a new one.
DROP TABLE IF EXISTS org_ci_credentials;
