-- Installs pg_stat_statements so a perf investigation can read per-statement
-- timings (SELECT ... FROM pg_stat_statements ORDER BY total_exec_time DESC).
--
-- docker-compose.prod.yml preloads the library (shared_preload_libraries); this
-- file creates the extension that exposes it. Runs ONLY on first init of an
-- empty data volume (postgres entrypoint behaviour), as the superuser, against
-- POSTGRES_DB. An existing deployment runs the same statement once by hand,
-- connected as the superuser to the application database:
--
--   CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
--
-- The local-dev stack (docker-compose.yml) does not preload the library: the
-- extension still installs there (only its view errors when queried), but any
-- failure is downgraded to a NOTICE so an init-script error can never leave a
-- fresh dev volume half-initialised.
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'db-init: pg_stat_statements not installed: %', SQLERRM;
END
$$;
