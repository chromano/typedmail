-- Which provider delivered each message, and the provider's own ID for it
-- (Resend's email_id is needed to fetch the body, which its webhook omits).
ALTER TABLE messages
    ADD COLUMN provider    text NOT NULL DEFAULT 'postmark'
                           CHECK (provider IN ('postmark', 'resend')),
    ADD COLUMN provider_id text NOT NULL DEFAULT '';

ALTER TABLE messages ALTER COLUMN provider DROP DEFAULT;
