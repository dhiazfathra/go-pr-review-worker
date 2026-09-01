BIN := bin/pr-review-worker
PKG := ./...

.PHONY: build test cover race lint fmt fmt-check vet check run clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/pr-review-worker

test:
	go test $(PKG)

cover:
	go test $(PKG) -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

race:
	go test -race $(PKG)

lint:
	golangci-lint run

fmt:
	gofmt -w ./cmd ./internal

fmt-check:
	@test -z "$$(gofmt -l ./cmd ./internal)" || { gofmt -l ./cmd ./internal; exit 1; }

vet:
	go vet $(PKG)

check: fmt-check vet lint race

run: build
	./$(BIN)

clean:
	go clean
	rm -f coverage.out
	rm -f $(BIN)
