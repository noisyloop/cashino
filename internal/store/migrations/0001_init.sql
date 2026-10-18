-- cashino schema v1.
-- The events table is the single source of truth. Every other table except
-- configuration (machines, users, bonus_rules, progressive config columns)
-- and operational records (alerts, reconciliations, event_gaps, audit_log)
-- is a projection that can be rebuilt by replaying events in id order.

-- ---------------------------------------------------------------------------
-- Event log (append-only)
-- ---------------------------------------------------------------------------
CREATE TABLE events (
    id          BIGSERIAL PRIMARY KEY,
    source      TEXT        NOT NULL,          -- machine id or sys:*
    seq         BIGINT      NOT NULL CHECK (seq > 0),
    type        TEXT        NOT NULL,
    ts          TIMESTAMPTZ NOT NULL,          -- source time
    recv        TIMESTAMPTZ NOT NULL,          -- host receive time
    payload     JSONB       NOT NULL,          -- full event document
    UNIQUE (source, seq)
);
CREATE INDEX events_type_ts ON events (type, ts);
CREATE INDEX events_ts ON events (ts);

CREATE FUNCTION forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% rejected)', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER events_append_only
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER events_no_truncate
    BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Last sequence written per source (projection of events).
CREATE TABLE stream_positions (
    source   TEXT PRIMARY KEY,
    last_seq BIGINT NOT NULL
);

-- ---------------------------------------------------------------------------
-- Configuration
-- ---------------------------------------------------------------------------
CREATE TABLE machines (
    id         TEXT PRIMARY KEY,
    theme      TEXT NOT NULL DEFAULT '',
    zone       TEXT NOT NULL DEFAULT '',
    bank       TEXT NOT NULL DEFAULT '',
    denom      BIGINT NOT NULL DEFAULT 1,
    rtp        DOUBLE PRECISION NOT NULL,
    x          INT NOT NULL DEFAULT 0,
    y          INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,                -- bcrypt
    role          TEXT NOT NULL CHECK (role IN ('attendant','slot_tech','accounting','compliance','admin')),
    disabled      BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE bonus_rules (
    id         TEXT PRIMARY KEY,
    definition JSONB NOT NULL,
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Progressive banks: configuration columns plus projected value columns.
CREATE TABLE progressive_banks (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    base            BIGINT NOT NULL,
    rate_bp         BIGINT NOT NULL,
    ceiling         BIGINT NOT NULL DEFAULT 0,
    current_milli   BIGINT NOT NULL,
    contributed_milli BIGINT NOT NULL DEFAULT 0,
    hits            BIGINT NOT NULL DEFAULT 0,
    last_hit_at     TIMESTAMPTZ,
    last_hit_amount BIGINT NOT NULL DEFAULT 0,
    last_hit_by     TEXT NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- Projections
-- ---------------------------------------------------------------------------
CREATE TABLE machine_meters (
    machine_id       TEXT PRIMARY KEY REFERENCES machines(id),
    coin_in          BIGINT NOT NULL DEFAULT 0,
    coin_out         BIGINT NOT NULL DEFAULT 0,
    jackpot          BIGINT NOT NULL DEFAULT 0,
    handpay          BIGINT NOT NULL DEFAULT 0,
    games_played     BIGINT NOT NULL DEFAULT 0,
    games_won        BIGINT NOT NULL DEFAULT 0,
    bills_in         BIGINT NOT NULL DEFAULT 0,
    tickets_in       BIGINT NOT NULL DEFAULT 0,
    tickets_out      BIGINT NOT NULL DEFAULT 0,
    ticket_in_count  BIGINT NOT NULL DEFAULT 0,
    ticket_out_count BIGINT NOT NULL DEFAULT 0,
    credits          BIGINT NOT NULL DEFAULT 0,
    theo_milli       BIGINT NOT NULL DEFAULT 0,
    last_seq         BIGINT NOT NULL DEFAULT 0,
    last_event_ts    TIMESTAMPTZ,
    last_recv        TIMESTAMPTZ,
    tilt             TEXT NOT NULL DEFAULT '',
    door             TEXT NOT NULL DEFAULT ''
);

CREATE TABLE machine_daily (
    machine_id  TEXT NOT NULL,
    day         DATE NOT NULL,
    coin_in     BIGINT NOT NULL DEFAULT 0,
    coin_out    BIGINT NOT NULL DEFAULT 0,
    jackpot     BIGINT NOT NULL DEFAULT 0,
    handpay     BIGINT NOT NULL DEFAULT 0,
    games       BIGINT NOT NULL DEFAULT 0,
    bills_in    BIGINT NOT NULL DEFAULT 0,
    tickets_in  BIGINT NOT NULL DEFAULT 0,
    tickets_out BIGINT NOT NULL DEFAULT 0,
    theo_milli  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (machine_id, day)
);
CREATE INDEX machine_daily_day ON machine_daily (day);

CREATE TABLE players (
    id           BIGINT PRIMARY KEY,
    card         TEXT UNIQUE NOT NULL,
    name         TEXT NOT NULL,
    tier         TEXT NOT NULL,
    tier_milli   BIGINT NOT NULL DEFAULT 0,
    points_milli BIGINT NOT NULL DEFAULT 0,
    comp_milli   BIGINT NOT NULL DEFAULT 0,
    freeplay     BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ
);

CREATE TABLE player_sessions (
    id           BIGINT PRIMARY KEY,
    player_id    BIGINT NOT NULL,
    machine_id   TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ,
    coin_in      BIGINT NOT NULL DEFAULT 0,
    coin_out     BIGINT NOT NULL DEFAULT 0,
    games        BIGINT NOT NULL DEFAULT 0,
    theo_milli   BIGINT NOT NULL DEFAULT 0,
    points_milli BIGINT NOT NULL DEFAULT 0,
    awards       TEXT[] NOT NULL DEFAULT '{}',
    closed_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX player_sessions_player ON player_sessions (player_id, started_at DESC);
CREATE INDEX player_sessions_open ON player_sessions (machine_id) WHERE ended_at IS NULL;

CREATE TABLE tickets (
    validation  TEXT PRIMARY KEY,
    amount      BIGINT NOT NULL CHECK (amount > 0),
    status      TEXT NOT NULL CHECK (status IN ('issued','redeemed','expired','voided')),
    issued_by   TEXT NOT NULL,
    issued_at   TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    redeemed_by TEXT NOT NULL DEFAULT '',
    redeemed_at TIMESTAMPTZ
);
CREATE INDEX tickets_outstanding ON tickets (issued_at) WHERE status = 'issued';

-- Ticket ledger: one row per issue/redeem/expire/void. A partial unique
-- index makes a second redemption of the same ticket impossible at the
-- database level, independent of application logic.
CREATE TABLE ticket_ledger (
    id         BIGSERIAL PRIMARY KEY,
    validation TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('issue','redeem','expire','void')),
    amount     BIGINT NOT NULL,
    liability_delta BIGINT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    ref        TEXT NOT NULL,
    event_id   BIGINT
);
CREATE UNIQUE INDEX ticket_ledger_one_close ON ticket_ledger (validation) WHERE kind <> 'issue';
CREATE UNIQUE INDEX ticket_ledger_one_issue ON ticket_ledger (validation) WHERE kind = 'issue';

CREATE TABLE progressive_hits (
    id       BIGSERIAL PRIMARY KEY,
    bank     TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    claim    TEXT UNIQUE NOT NULL,
    amount   BIGINT NOT NULL,
    at       TIMESTAMPTZ NOT NULL,
    event_id BIGINT
);

-- ---------------------------------------------------------------------------
-- Operational records
-- ---------------------------------------------------------------------------
CREATE TABLE alerts (
    id          BIGINT PRIMARY KEY,
    type        TEXT NOT NULL,
    severity    TEXT NOT NULL,
    machine_id  TEXT NOT NULL,
    message     TEXT NOT NULL,
    state       TEXT NOT NULL,
    count       BIGINT NOT NULL,
    opened_at   TIMESTAMPTZ NOT NULL,
    last_at     TIMESTAMPTZ NOT NULL,
    acked_by    TEXT NOT NULL DEFAULT '',
    acked_at    TIMESTAMPTZ,
    ack_reason  TEXT NOT NULL DEFAULT '',
    resolved_by TEXT NOT NULL DEFAULT '',
    resolved_at TIMESTAMPTZ,
    note        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX alerts_open ON alerts (state) WHERE state <> 'resolved';

CREATE TABLE reconciliations (
    id         BIGSERIAL PRIMARY KEY,
    machine_id TEXT NOT NULL,
    as_of_seq  BIGINT NOT NULL,
    host_seq   BIGINT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    ok         BOOLEAN NOT NULL,
    trigger    TEXT NOT NULL,
    mismatches JSONB NOT NULL,
    reported   JSONB NOT NULL,
    host       JSONB NOT NULL
);
CREATE INDEX reconciliations_machine ON reconciliations (machine_id, at DESC);

CREATE TABLE event_gaps (
    id          BIGSERIAL PRIMARY KEY,
    machine_id  TEXT NOT NULL,
    from_seq    BIGINT NOT NULL,
    to_seq      BIGINT NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL,
    recovered   BOOLEAN NOT NULL
);

-- Audit log: append-only and hash-chained. Each row's hash covers its own
-- content and the previous row's hash, so any edit made by bypassing the
-- trigger (e.g. as a superuser) is detectable by /api/audit/verify.
CREATE TABLE audit_log (
    id        BIGSERIAL PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL,
    actor     TEXT NOT NULL,
    role      TEXT NOT NULL,
    action    TEXT NOT NULL,
    target    TEXT NOT NULL,
    reason    TEXT NOT NULL,
    details   JSONB NOT NULL,
    outcome   TEXT NOT NULL,
    remote    TEXT NOT NULL DEFAULT '',
    prev_hash TEXT NOT NULL,
    hash      TEXT NOT NULL
);
CREATE INDEX audit_log_at ON audit_log (at DESC);
CREATE TRIGGER audit_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER audit_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();
