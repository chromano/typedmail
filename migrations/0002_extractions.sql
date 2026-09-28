-- The JSON extracted from each message. One row per message: extracting a
-- message again (e.g. after make requeue) replaces its result.
CREATE TABLE extractions (
    message_id  bigint PRIMARY KEY REFERENCES messages (id),
    -- The model that produced the result; with refusal fallbacks it can
    -- differ from the configured one.
    model       text NOT NULL,
    data        jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
