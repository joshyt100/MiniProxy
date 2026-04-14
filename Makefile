
.PHONY: all deps fmt vet lint test build ci

GO := go

all: run

deps:
	$(GO) mod download

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run --timeout=2m

test:
	$(GO) test -race ./... -v

build:
	$(GO) build ./...

run: deps fmt vet lint test build
