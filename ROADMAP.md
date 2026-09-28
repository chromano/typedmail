# Roadmap

Each unchecked item is meant to become one GitHub issue.

## Ingestion

- [x] Postmark inbound webhook with basic auth and a 35 MB body limit
- [x] Store message and enqueue `extract` job in one transaction
- [x] Dedupe on RFC 5322 `Message-ID`, falling back to Postmark's ID
  prefixed with `postmark:`
- [x] Route to inbox by mailbox hash or custom-domain local part
- [x] Embedded migrations applied on startup under an advisory lock
- [x] Answer 403 for payloads that can never be accepted, so Postmark stops
  retrying them (#2)
- [x] Strip NUL bytes from inbound payloads, which Postgres rejects (#3)
- [x] Resend inbound alongside Postmark: Svix-signed webhook, a `fetch` job
  for the body, dedupe across providers on the sender's Message-ID (#12)

## Worker

- [x] Worker pool that claims due jobs with `FOR UPDATE SKIP LOCKED` (#4)
- [x] Retries with backoff: 10 attempts over about 6 hours, `last_error`
  recorded
- [x] Reclaim jobs left `running` by a dead worker
- [x] `make requeue` to retry failed jobs

## Inboxes

- [x] Each inbox's output defined as a JSON Schema in `schemas/`, loaded by
  `make seed`: `orders`, `invoices`, `shipments` (#6)

## Extraction

- [x] Extract JSON matching the inbox schema from the email body with Claude,
  using structured outputs (#7)
- [x] Store results in the `extractions` table (#7, absorbs #8)
- [ ] Make the model answer every field: mark all properties required in the
  schema sent to the API, with optional ones nullable. Today it leaves out
  optional fields the email does state (a shipment's items and delivery
  date), and once corrupted a value (`tracking_url`)

## Docs

- [x] Running with Postmark and ngrok, in the README

## Later

Not needed to prove "email in, JSON out". Revisit once there is a first user.

- Validate every extraction against the full inbox schema, which enforces the
  constraints the API can't (`pattern`, `minimum`, `format`), and record
  failures (#9)
- CI: vet and tests (with Postgres) on every push (#10)
- Flag fields the system is unsure about and route the message to review
- Review UI: list messages waiting for review; show the email next to the
  extracted fields; edit, approve or reject; reviewer authentication
- Deliver approved JSON to the customer: outbound webhook with retries and
  signing, plus an API to fetch results
- API to create and update inboxes, replacing `make seed`
- Extract from attachments (PDF, images), not just the body
- Keep the history of extraction results
- `make test-docker`: run the test suite in a Go container so the host
  doesn't need Go
- Deploy to Cloud Run with managed Postgres
- Retention policy for raw payloads
