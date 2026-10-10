-- baki:compat expand
-- The background jobs' run state across replicas (ADR 0023): one row per job (lifecycle.Singleton), written by the
-- replica that runs the job and read by every replica that skips it, so a job no replica has run successfully for too
-- long is reported overdue, while a replica skipping because another holds the job's lock is normal. name is a
-- declared job name; holder_instance the replica that started the last run; last_error the last failure's error
-- text, kept as history after a later success. A new table only: the release before neither reads nor writes it.
CREATE TABLE IF NOT EXISTS job_runs (
    name            TEXT        NOT NULL,
    last_started_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_error_at   TIMESTAMPTZ,
    last_error      TEXT        NOT NULL DEFAULT '',
    holder_instance TEXT        NOT NULL DEFAULT '',
    CONSTRAINT job_runs_pkey PRIMARY KEY (name),
    CONSTRAINT job_runs_name_check CHECK (char_length(name) BETWEEN 1 AND 64)
);
