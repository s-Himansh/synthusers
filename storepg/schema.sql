-- synthusers Postgres schema. Idempotent: the store runs it on startup.
CREATE TABLE IF NOT EXISTS targets (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    spec       JSONB NOT NULL,          -- full synthusers.Target document
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS runs (
    id          BIGSERIAL PRIMARY KEY,
    target_id   TEXT NOT NULL,
    mode        TEXT NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    pages       INT NOT NULL DEFAULT 0,
    requests    INT NOT NULL DEFAULT 0,
    new_issues  INT NOT NULL DEFAULT 0,
    shard       INT NOT NULL DEFAULT 0,
    worker      TEXT NOT NULL DEFAULT '',
    persona     TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS runs_target_started ON runs (target_id, started_at DESC);
CREATE INDEX IF NOT EXISTS runs_started ON runs (started_at DESC);

CREATE TABLE IF NOT EXISTS issues (
    fingerprint TEXT PRIMARY KEY,
    target_id   TEXT NOT NULL,
    type        TEXT NOT NULL,
    severity    TEXT NOT NULL,
    url         TEXT NOT NULL DEFAULT '',
    url_key     TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open',   -- open | resolved
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    occurrences BIGINT NOT NULL DEFAULT 1,
    resolved_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS issues_status_severity ON issues (status, severity, last_seen DESC);
CREATE INDEX IF NOT EXISTS issues_target_status ON issues (target_id, status);
CREATE INDEX IF NOT EXISTS issues_type ON issues (type, status);
