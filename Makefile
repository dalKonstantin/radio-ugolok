BIN := bin/radio

.PHONY: run build test clean

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
