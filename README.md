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

Extraction calls Claude and needs `ANTHROPIC_API_KEY`, set in your shell or in
`.env`. Without it the app still receives email, but jobs stay queued until a
key is configured. `EXTRACT_MODEL` overrides the model (default
`claude-opus-5`).

```sh
make up      # build and start app + Postgres
make seed    # create an inbox per schemas/<slug>.json (orders, invoices, shipments)
make send    # post testdata/postmark_inbound.json → {"status":"accepted","id":1}
make send    # again                             → {"status":"duplicate"}
make send SAMPLE=testdata/postmark_invoice.json   # or postmark_shipment.json
make test    # unit + Postgres integration tests (needs Go)
make requeue # retry failed jobs from scratch (JOB=<id> for one)
make results # latest extractions next to their emails
```

## Receiving real email with Postmark and ngrok

Postmark receives the email and posts it to the webhook; ngrok gives your
local app a public HTTPS URL Postmark can reach. You need a Postmark account
with a server, and an ngrok account with its authtoken configured
(`ngrok config add-authtoken <token>`).

**1. Start the app with your own webhook password.** The default `dev-secret`
is in this repo, and anyone who finds your ngrok URL could post to the webhook
with it.

```sh
export INBOUND_PASSWORD=$(openssl rand -hex 16)   # hex, so it's safe in a URL
make up && make seed
```

Keep the variable exported in that shell: `make up` passes it to the app and
`make send` uses it too. Set `ANTHROPIC_API_KEY` as described above, or
messages are stored but not extracted.

**2. Expose it with ngrok** in another terminal:

```sh
ngrok http 8080
```

Copy the `https://…` address from the `Forwarding` line and check it reaches
the app: `curl https://<forwarding-host>/healthz` should print `ok`.

**3. Point Postmark at it.** In your Postmark server, open the inbound message
stream's settings and:

- set the inbound webhook URL, with the credentials in it:

  ```
  https://postmark:<INBOUND_PASSWORD>@<forwarding-host>/webhooks/postmark
  ```

- note the inbound address shown there, `<hash>@inbound.postmarkapp.com`.

**4. Send an email** from any mailbox to the inbox you want, by adding its
name after a `+`:

```
<hash>+orders@inbound.postmarkapp.com
<hash>+invoices@inbound.postmarkapp.com
<hash>+shipments@inbound.postmarkapp.com
```

**5. Watch it arrive.**

- `http://localhost:4040` is ngrok's inspector: it shows each webhook request
  from Postmark and the app's response.
- `docker compose logs -f app` shows `message accepted`, then `extracted` once
  the worker is done.
- `make results` shows the email next to the extracted JSON.

### When it doesn't work

| What you see | Why |
|---|---|
| `401` in the ngrok inspector | The password in the webhook URL doesn't match `INBOUND_PASSWORD`. After changing it, run `make up` again. |
| `{"status":"dropped"}` | No inbox with that name: the address has no `+inbox` part, or `make seed` hasn't run. |
| `403` | The payload can never be accepted (no sender or Message-ID). Postmark stops retrying. |
| Postmark can't reach the webhook | ngrok isn't running, or its URL changed: free ngrok URLs change on every restart unless you use your static domain, so update the webhook URL. |
| Message stored, job stays `queued` | The app has no `ANTHROPIC_API_KEY`; the log says so at startup. |
| Job `failed` | `docker compose logs app \| grep 'job failed'` shows why. After fixing the cause, `make requeue`. |

If the app is down when email arrives, Postmark retries the webhook for
about 10 hours, so messages arrive once it's back up.
