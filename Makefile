DATABASE_URL      ?= postgres://typedmail:typedmail@localhost:5433/typedmail?sslmode=disable
TEST_DATABASE_URL ?= postgres://typedmail:typedmail@localhost:5433/typedmail_test?sslmode=disable
INBOUND_USER      ?= postmark
INBOUND_PASSWORD  ?= dev-secret

export DATABASE_URL TEST_DATABASE_URL INBOUND_USER INBOUND_PASSWORD

.PHONY: db up down run test seed send requeue results

db:    ## start Postgres only
	docker compose up -d --wait db

up:    ## build and run app + Postgres in Docker
	docker compose up -d --build --wait

down:
	docker compose down

run: db ## run the app on the host (needs Go)
	go run ./cmd/typedmail

test: db ## unit + Postgres integration tests
	go test ./...

seed:  ## create or update one inbox per schemas/<slug>.json
	@for f in schemas/*.json; do \
		slug=$$(basename $$f .json); \
		echo "seeding $$slug"; \
		echo "INSERT INTO inboxes (slug, schema) VALUES (:'slug', :'schema'::jsonb) \
			ON CONFLICT (slug) DO UPDATE SET schema = EXCLUDED.schema" | \
		docker compose exec -T db psql -U typedmail -q -v ON_ERROR_STOP=1 \
			-v slug="$$slug" -v schema="$$(cat $$f)" || exit 1; \
	done

SAMPLE ?= testdata/postmark_inbound.json

send:  ## post a sample email to the webhook (SAMPLE=testdata/postmark_invoice.json)
	curl -s -u $(INBOUND_USER):$(INBOUND_PASSWORD) \
		-H 'Content-Type: application/json' \
		--data @$(SAMPLE) \
		http://localhost:8080/webhooks/postmark; echo

requeue:  ## requeue failed jobs with a fresh set of attempts (JOB=<id> for one)
	@echo "UPDATE jobs SET status = 'queued', attempts = 0, run_at = now(), updated_at = now() \
		WHERE status = 'failed' AND id = coalesce(nullif(:'job', '')::bigint, id) RETURNING 'requeued job ' || id" | \
	docker compose exec -T db psql -U typedmail -q -At -v ON_ERROR_STOP=1 -v job="$(JOB)"

results:  ## show the latest extractions next to their emails
	@docker compose exec -T db psql -U typedmail -c \
		"SELECT m.id, i.slug AS inbox, m.from_address, m.subject, j.status, e.model, jsonb_pretty(e.data) AS extracted \
		FROM messages m JOIN inboxes i ON i.id = m.inbox_id JOIN jobs j ON j.message_id = m.id \
		LEFT JOIN extractions e ON e.message_id = m.id ORDER BY m.id DESC LIMIT 5"
