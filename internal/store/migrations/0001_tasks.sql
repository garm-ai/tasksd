-- The three tables tasksd owns: the queue, its audit trail, and the triage
-- an agent or a person adds to a task without deciding it.
--
-- One task row is the whole of what a person approves: the tool it is for,
-- the subject it names, the material values the digest is computed over, and
-- the predicate that says who may act on it. Everything else on the row is
-- what happened to it.

CREATE TABLE IF NOT EXISTS tasks (
    id               text PRIMARY KEY,
    -- The tenant comes from Garm-Invocation on every call and is never a
    -- request field. It is on the row because a task outlives the call that
    -- opened it and nothing else here records which tenant it belongs to.
    tenant           text        NOT NULL,
    kind             text        NOT NULL,
    -- The run that asked, and the person it asked for. The run id is where
    -- the decision is signalled; the requester is who may not decide it.
    run_id           text        NOT NULL,
    requester        text        NOT NULL,
    tool_fqn         text        NOT NULL DEFAULT '',
    subject          text        NOT NULL DEFAULT '',
    -- Material is JSON in the clear: it is the digest's input and the card's
    -- facts, and a value a person cannot read is a value they cannot
    -- approve. The canary test asserts no credential ever lands in it.
    material         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- The digest over the material, stored so a retry of the same ask finds
    -- the same task instead of opening a second one.
    material_digest  text        NOT NULL DEFAULT '',
    predicate        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    state            text        NOT NULL,
    claimant         text        NOT NULL DEFAULT '',
    claim_expires_at timestamptz,
    decision         text        NOT NULL DEFAULT '',
    reason           text        NOT NULL DEFAULT '',
    -- Who decided. On a triage decline this is the agent, and on_behalf_of
    -- is the person it was acting for.
    decided_by       text        NOT NULL DEFAULT '',
    on_behalf_of     text        NOT NULL DEFAULT '',
    grant_jti        text        NOT NULL DEFAULT '',
    -- The one column in this schema that holds a credential, and it holds a
    -- spent one: the grant the approver presented, kept for the audit trail.
    -- Everything else is checked by the canary test.
    "grant"          text        NOT NULL DEFAULT '',
    decision_type    text        NOT NULL DEFAULT '',
    question         text        NOT NULL DEFAULT '',
    catalogue_digest text        NOT NULL DEFAULT '',
    -- The answer to an ASK, as the decision message the asker declared.
    answer           bytea,
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    decided_at       timestamptz,
    CONSTRAINT tasks_kind_check  CHECK (kind IN ('APPROVAL','ASK')),
    CONSTRAINT tasks_state_check CHECK (state IN ('OPEN','CLAIMED','APPROVED','DECLINED','ANSWERED','EXPIRED')),
    -- A row that records a decision may not also say it is waiting for one.
    CONSTRAINT tasks_decision_state_check CHECK (
        decision = '' OR state IN ('APPROVED','DECLINED','ANSWERED')
    )
);

-- The queue is read per tenant, newest first. The id is a ULID, so this
-- index is also the time ordering.
CREATE INDEX IF NOT EXISTS tasks_tenant_state_idx ON tasks (tenant, state, id DESC);
CREATE INDEX IF NOT EXISTS tasks_tenant_run_idx   ON tasks (tenant, run_id);

-- One ask, one task. A runner that retries create_task after a timeout it
-- never saw the answer to must not open a second approval for the same
-- payment, so the same run asking for the same tool with the same material
-- finds the row it already has.
CREATE UNIQUE INDEX IF NOT EXISTS tasks_idempotency_idx
    ON tasks (tenant, run_id, tool_fqn, material_digest);

CREATE TABLE IF NOT EXISTS task_events (
    task_id text        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq     integer     NOT NULL,
    at      timestamptz NOT NULL,
    actor   text        NOT NULL,
    kind    text        NOT NULL,
    detail  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (task_id, seq)
);

CREATE TABLE IF NOT EXISTS triage (
    task_id        text        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq            integer     NOT NULL,
    at             timestamptz NOT NULL,
    actor          text        NOT NULL,
    action         text        NOT NULL,
    recommendation text        NOT NULL DEFAULT '',
    reason         text        NOT NULL DEFAULT '',
    PRIMARY KEY (task_id, seq)
);
