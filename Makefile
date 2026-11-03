BIN := bin/radio

.PHONY: run build test clean up down logs

run:
	go run ./cmd/radio -addr localhost:8080

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/radio

test:
	go vet ./...
	go test ./...
	node --test test/

clean:
	rm -rf bin

# Docker: собрать образ и поднять в фоне на :8080, данные в ./data.
up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f radio
