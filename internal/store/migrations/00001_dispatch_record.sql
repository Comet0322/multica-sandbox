-- +goose Up
CREATE TABLE pods (
    id         text PRIMARY KEY,
    state      text NOT NULL CHECK (state IN ('ready', 'gone')),
    phase      text NOT NULL,
    tenant     text NOT NULL DEFAULT '',
    kind       text NOT NULL DEFAULT '',
    key        text NOT NULL DEFAULT '',
    since      timestamptz,
    until      timestamptz,
    created_at timestamptz,
    ready_at   timestamptz,
    gone_at    timestamptz,
    gone_why   text NOT NULL DEFAULT ''
);
CREATE INDEX pods_held ON pods (ready_at, id) WHERE state <> 'gone';
CREATE INDEX pods_gone ON pods (gone_at) WHERE state = 'gone';

CREATE TABLE tasks (
    id           text PRIMARY KEY,
    tenant       text NOT NULL,
    kind         text NOT NULL,
    key          text NOT NULL DEFAULT '',
    pod          text,
    state        text NOT NULL CHECK (state IN ('placed', 'handed', 'running', 'ended')),
    payload      bytea CHECK (payload IS NULL OR state IN ('placed', 'handed')),
    claimed_at   timestamptz NOT NULL,
    started_at   timestamptz,
    ended_at     timestamptz,
    end_reason   text NOT NULL DEFAULT '',
    fail_reason  text NOT NULL DEFAULT '',
    fail_pending boolean NOT NULL DEFAULT false
);
CREATE INDEX tasks_open ON tasks (claimed_at, id) WHERE state <> 'ended' OR fail_pending;
CREATE INDEX tasks_done ON tasks (ended_at) WHERE state = 'ended' AND NOT fail_pending;

-- +goose Down
DROP TABLE tasks;
DROP TABLE pods;
