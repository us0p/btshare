# `make check` runs the same checks as CI (.github/workflows/ci.yml).
# Tool versions match CI; they're fetched with `go run` on first use.
GOLANGCI_LINT = go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOVULNCHECK   = go run golang.org/x/vuln/cmd/govulncheck@v1.9.0

build:
	go build -o bin/btshare ./cmd/btshare

test:
	go test -race -count=1 ./...

# Rewrites files: gofmt + goimports, as configured in .golangci.yml.
fmt:
	$(GOLANGCI_LINT) fmt ./...

lint:
	$(GOLANGCI_LINT) run ./...

shellcheck:
	shellcheck install.sh

vulncheck:
	$(GOVULNCHECK) ./...

tidy-check:
	go mod tidy -diff
	go mod verify

check: lint test tidy-check shellcheck vulncheck

install:
	./install.sh

uninstall:
	./install.sh --uninstall

.PHONY: build test fmt lint shellcheck vulncheck tidy-check check install uninstall
