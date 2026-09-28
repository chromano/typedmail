-- An inbox is one inbound address with its own extraction schema.
-- Postmark delivers mail for <hash>+<slug>@inbound.postmarkapp.com and passes
-- <slug> as MailboxHash, which is how a message finds its inbox.
CREATE TABLE inboxes (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug        text NOT NULL UNIQUE,
    schema      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE messages (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    inbox_id      bigint NOT NULL REFERENCES inboxes (id),
    -- RFC 5322 Message-ID when present, otherwise the provider's own ID
    -- prefixed with the provider name (e.g. "postmark:<id>").
    -- Unique per inbox, so provider retries and sender resends are dropped.
    message_id    text NOT NULL,
    from_address  text NOT NULL,
    subject       text NOT NULL DEFAULT '',
    text_body     text NOT NULL DEFAULT '',
    html_body     text NOT NULL DEFAULT '',
    raw           jsonb NOT NULL,
    received_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (inbox_id, message_id)
);

-- Work queue drained by the worker pool with SELECT ... FOR UPDATE SKIP LOCKED.
CREATE TABLE jobs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        text NOT NULL,
    message_id  bigint NOT NULL REFERENCES messages (id),
    status      text NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts    int NOT NULL DEFAULT 0,
    run_at      timestamptz NOT NULL DEFAULT now(),
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobs_ready_idx ON jobs (run_at) WHERE status = 'queued';
