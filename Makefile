.PHONY: build build-tagger build-tray test test-tagger test-race lint coverage coverage-tagger

LDFLAGS := -ldflags="$(shell . ./packaging/release-env.sh && printf '%s' "$$LDFLAGS")"

build:
	go build $(LDFLAGS) ./cmd/monbooru

build-tagger:
	go build -tags tagger $(LDFLAGS) ./cmd/monbooru

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monbooru

test:
	go test -timeout 30m ./...

test-tagger:
	go test -tags tagger -timeout 30m ./...

test-race:
	go test -race -timeout 30m ./...

lint:
	golangci-lint run

coverage:
	go test -coverpkg=./... -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage.out -o coverage.html

coverage-tagger:
	go test -tags tagger -coverpkg=./... -coverprofile=coverage-tagger.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage-tagger.out -o coverage-tagger.html