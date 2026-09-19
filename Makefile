.PHONY: run test fmt vet docker-up docker-down docker-logs docker-ps migrate-up migrate-down db-backup

run:
	go run ./cmd/bot

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

docker-up:
	docker compose up --build -d

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f bot

docker-ps:
	docker compose ps

migrate-up:
	docker compose run --rm migrator

migrate-down:
	set -a; . ./.env; set +a; docker compose run --rm migrator -path=/migrations -database="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@postgres:5432/$${POSTGRES_DB}?sslmode=disable" down 1

db-backup:
	./scripts/db-backup.sh
