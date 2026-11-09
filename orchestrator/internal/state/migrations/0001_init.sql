-- ACO state store, schema version 1.
-- Times are Unix milliseconds (UTC). Free text is masked before it is written.

CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    repo_key       TEXT NOT NULL,
    repo_path      TEXT NOT NULL,
    base_branch    TEXT NOT NULL DEFAULT '',
    run_branch     TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL CHECK (mode IN ('full', 'plan_only')),
    input_kind     TEXT NOT NULL CHECK (input_kind IN ('requirements', 'plan')),
    status         TEXT NOT NULL,
    paused_from    TEXT NOT NULL DEFAULT '',
    resume_at      INTEGER,
    reason_class   TEXT NOT NULL DEFAULT '',
    reason_stage   TEXT NOT NULL DEFAULT '',
    reason_phase   INTEGER NOT NULL DEFAULT 0,
    reason_detail  TEXT NOT NULL DEFAULT '',
    plan_hash      TEXT NOT NULL DEFAULT '',
    current_phase  INTEGER NOT NULL DEFAULT 0,
    last_event_seq INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    ended_at       INTEGER
);
CREATE INDEX runs_by_repo ON runs (repo_key, created_at);

-- One row per entry into a stage (plan_review is entered once per round).
CREATE TABLE stages (
    id         INTEGER PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs (id),
    stage      TEXT NOT NULL,
    entry      INTEGER NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('active', 'completed', 'blocked', 'failed', 'cancelled')),
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    UNIQUE (run_id, stage, entry)
);

CREATE TABLE plan_reviews (
    id             INTEGER PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs (id),
    round          INTEGER NOT NULL,
    reviewer_model TEXT NOT NULL,
    verdict        TEXT NOT NULL,
    gate_passed    INTEGER NOT NULL,
    review_json    TEXT NOT NULL,
    artifact       TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    UNIQUE (run_id, round)
);

CREATE TABLE phases (
    run_id        TEXT NOT NULL REFERENCES runs (id),
    number        INTEGER NOT NULL,
    title         TEXT NOT NULL,
    status        TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    base_commit   TEXT NOT NULL DEFAULT '',
    commit_sha    TEXT NOT NULL DEFAULT '',
    reason_class  TEXT NOT NULL DEFAULT '',
    reason_detail TEXT NOT NULL DEFAULT '',
    started_at    INTEGER,
    ended_at      INTEGER,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (run_id, number)
);

CREATE TABLE attempts (
    run_id     TEXT NOT NULL,
    phase      INTEGER NOT NULL,
    number     INTEGER NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('active', 'exhausted', 'committed', 'blocked')),
    session_id TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    PRIMARY KEY (run_id, phase, number),
    FOREIGN KEY (run_id, phase) REFERENCES phases (run_id, number)
);

-- Intent/outcome log (§14.2). phase/attempt are 0 for run-level steps.
CREATE TABLE steps (
    id              INTEGER PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES runs (id),
    phase           INTEGER NOT NULL DEFAULT 0,
    attempt         INTEGER NOT NULL DEFAULT 0,
    kind            TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('started', 'completed', 'failed', 'needs_reconciliation', 'reconciled')),
    owner           TEXT NOT NULL,
    intent_json     TEXT NOT NULL,
    checkpoint_ref  TEXT NOT NULL DEFAULT '',
    outcome_json    TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    resolution      TEXT NOT NULL DEFAULT '',
    started_at      INTEGER NOT NULL,
    ended_at        INTEGER,
    UNIQUE (run_id, idempotency_key)
);
CREATE INDEX steps_by_status ON steps (run_id, status);

CREATE TABLE invocations (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs (id),
    step_id       INTEGER REFERENCES steps (id),
    agent         TEXT NOT NULL CHECK (agent IN ('claude', 'codex')),
    role          TEXT NOT NULL CHECK (role IN ('planner', 'implementer', 'plan_reviewer', 'reviewer')),
    model         TEXT NOT NULL,
    auth_mode     TEXT NOT NULL CHECK (auth_mode IN ('subscription', 'api')),
    session_id    TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
    turns         INTEGER NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd      REAL NOT NULL DEFAULT 0,
    transcript    TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER
);

CREATE TABLE gate_results (
    id           INTEGER PRIMARY KEY,
    run_id       TEXT NOT NULL REFERENCES runs (id),
    phase        INTEGER NOT NULL DEFAULT 0,
    attempt      INTEGER NOT NULL DEFAULT 0,
    round        INTEGER NOT NULL DEFAULT 0,
    gate         TEXT NOT NULL CHECK (gate IN ('typecheck', 'lint', 'unit', 'integration', 'build', 'security')),
    is_baseline  INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'not_applicable', 'error')),
    command      TEXT NOT NULL DEFAULT '',
    exit_code    INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    tree_hash    TEXT NOT NULL DEFAULT '',
    summary_json TEXT NOT NULL DEFAULT '{}',
    log_artifact TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX gate_results_by_phase ON gate_results (run_id, phase, attempt, round);

CREATE TABLE reviews (
    id             INTEGER PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs (id),
    kind           TEXT NOT NULL CHECK (kind IN ('code', 'final')),
    phase          INTEGER NOT NULL DEFAULT 0,
    attempt        INTEGER NOT NULL DEFAULT 0,
    round          INTEGER NOT NULL,
    reviewer_model TEXT NOT NULL,
    verdict        TEXT NOT NULL,
    gate_passed    INTEGER NOT NULL,
    tree_hash      TEXT NOT NULL DEFAULT '',
    review_json    TEXT NOT NULL,
    artifact       TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    UNIQUE (run_id, kind, phase, attempt, round)
);

-- Plan findings (PF-NNN) and code findings (F-NN-NNN) share one ledger.
CREATE TABLE findings (
    run_id           TEXT NOT NULL REFERENCES runs (id),
    id               TEXT NOT NULL,
    scope            TEXT NOT NULL CHECK (scope IN ('plan', 'code', 'final')),
    phase            INTEGER NOT NULL DEFAULT 0,
    fingerprint      TEXT NOT NULL,
    severity         TEXT NOT NULL,
    category         TEXT NOT NULL,
    resolution_class TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL,
    title            TEXT NOT NULL,
    file             TEXT NOT NULL DEFAULT '',
    line_start       INTEGER NOT NULL DEFAULT 0,
    line_end         INTEGER NOT NULL DEFAULT 0,
    opened_round     INTEGER NOT NULL,
    closed_round     INTEGER NOT NULL DEFAULT 0,
    reopen_count     INTEGER NOT NULL DEFAULT 0,
    dispute_count    INTEGER NOT NULL DEFAULT 0,
    data_json        TEXT NOT NULL DEFAULT '{}',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (run_id, id)
);
CREATE INDEX findings_by_fingerprint ON findings (run_id, fingerprint);

CREATE TABLE commits (
    run_id        TEXT NOT NULL REFERENCES runs (id),
    sha           TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('plan_freeze', 'phase', 'final_fix', 'final_report')),
    phase         INTEGER NOT NULL DEFAULT 0,
    attempt       INTEGER NOT NULL DEFAULT 0,
    tree_hash     TEXT NOT NULL,
    reviewed_tree TEXT NOT NULL DEFAULT '',
    tag           TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (run_id, sha)
);

CREATE TABLE reports (
    id         INTEGER PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs (id),
    kind       TEXT NOT NULL CHECK (kind IN ('phase', 'blocked', 'failure', 'final')),
    phase      INTEGER NOT NULL DEFAULT 0,
    path       TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    commit_sha TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (run_id, kind, phase)
);

CREATE TABLE events (
    run_id  TEXT NOT NULL REFERENCES runs (id),
    seq     INTEGER NOT NULL,
    type    TEXT NOT NULL,
    time    INTEGER NOT NULL,
    stage   TEXT NOT NULL DEFAULT '',
    phase   INTEGER NOT NULL DEFAULT 0,
    payload TEXT NOT NULL DEFAULT '{}',
    message TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, seq)
);

-- One active run per repository (§14.2).
CREATE TABLE locks (
    repo_key     TEXT PRIMARY KEY,
    run_id       TEXT NOT NULL,
    owner        TEXT NOT NULL,
    pid          INTEGER NOT NULL,
    host         TEXT NOT NULL,
    acquired_at  INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL,
    ttl_ms       INTEGER NOT NULL
);
