# typedmail

Send any business email to an address and get back validated, structured JSON.
Anything the system isn't sure about goes to a person to check before it's used.

> Work in progress

## How it works

```
Postmark inbound ──webhook──▶ POST /webhooks/postmark
                                 │  basic auth, 35 MB limit
                                 │  dedupe on the sender's Message-ID
                                 ▼
                    Postgres: messages + jobs (one transaction)
                                 │
                                 ▼
                    worker claims the extract job
                                 │
                                 ▼
                    LLM extracts JSON using the inbox schema
                                 │
                                 ▼
                    validate against the schema → extractions
                                 │
                   ┌─────────────┴─────────────┐
                   │ unsure                    │ confident
                   ▼                           ▼
             human review ──approved──▶ deliver JSON to the customer
```

- **Fast acknowledgement.** The webhook only stores the message and enqueues an
  `extract` job. Nothing slow happens while the provider waits.
- **No duplicates.** Messages are unique per inbox on the RFC 5322 `Message-ID`
  header (falling back to Postmark's ID), so provider retries and sender resends
  are stored once.
- **Inbox routing.** `<hash>+orders@inbound.postmarkapp.com` routes to the
  `orders` inbox. With a custom inbound domain, `orders@mail.example.com` does too.
- **One schema per inbox.** Each inbox defines the JSON it produces as a JSON
  Schema, so `orders` and `invoices` can return different shapes.
- **Background work.** Workers claim jobs with `FOR UPDATE SKIP LOCKED`, so
  several can run in parallel, and failed jobs are retried with backoff.
- **LLM extraction.** An LLM reads the email and fills in the inbox schema.
- **Validated output.** Every extraction is checked against the schema before
  it is used, and results are kept in `extractions`.
- **Human review.** Anything the system isn't sure about goes to a person,
  who can edit, approve or reject it.
- **Delivery.** Approved JSON is sent to the customer by a signed webhook and
  can also be fetched through an API.
- **Self-migrating.** SQL migrations are embedded in the binary and applied on
  startup under a Postgres advisory lock, so parallel instances don't race.

See [ROADMAP.md](ROADMAP.md) for planned work.

## Running locally

Requires Docker. Go 1.25+ is only needed to run outside Docker.

```sh
make up      # build and start app + Postgres
make seed    # create the "orders" inbox
make send    # post testdata/postmark_inbound.json → {"status":"accepted","id":1}
make send    # again                             → {"status":"duplicate"}
make test    # unit + Postgres integration tests (needs Go)
```
