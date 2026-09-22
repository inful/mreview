# mreview dev Makefile
#
# Convenience wrappers for the day-to-day commands. CI runs the same
# commands directly (see .github/workflows/ci.yml) — this file exists
# so contributors can run `make test`, `make lint`, etc. without
# remembering the exact flags.

.PHONY: build test test-race lint tidy vet fmt run clean help

BINARY := dist/mreview
PKG    := ./cmd/mreview

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-15s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build static binary into dist/mreview.
	go build -trimpath -ldflags="-s -w" -o $(BINARY) $(PKG)

test: ## Run unit tests (race-enabled, no cache).
	go test -race -count=1 ./...

test-race: ## Alias for test.
	go test -race -count=1 ./...

lint: ## Run golangci-lint with the project's config.
	golangci-lint run

tidy: ## Run go mod tidy and verify go.sum is consistent.
	go mod tidy
	git diff --exit-code go.mod go.sum

vet: ## Run go vet.
	go vet ./...

fmt: ## Run gofmt + goimports.
	gofmt -s -w .
	goimports -local github.com/inful/mreview -w .

run: ## Run mreview (pass ARGS="..." for subcommand + flags).
	go run $(PKG) $(ARGS)

clean: ## Remove built artifacts.
	rm -rf dist/
