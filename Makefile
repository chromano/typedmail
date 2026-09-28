DATABASE_URL      ?= postgres://typedmail:typedmail@localhost:5433/typedmail?sslmode=disable
TEST_DATABASE_URL ?= postgres://typedmail:typedmail@localhost:5433/typedmail_test?sslmode=disable
INBOUND_USER      ?= postmark
INBOUND_PASSWORD  ?= dev-secret

export DATABASE_URL TEST_DATABASE_URL INBOUND_USER INBOUND_PASSWORD

.PHONY: db up down run test seed send

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

seed:  ## create the "orders" inbox
	docker compose exec -T db psql -U typedmail -c \
		"INSERT INTO inboxes (slug) VALUES ('orders') ON CONFLICT DO NOTHING"

send:  ## post the sample email to the webhook
	curl -s -u $(INBOUND_USER):$(INBOUND_PASSWORD) \
		-H 'Content-Type: application/json' \
		--data @testdata/postmark_inbound.json \
		http://localhost:8080/webhooks/postmark; echo
