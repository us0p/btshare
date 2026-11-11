build:
	go build -o bin/btshare ./cmd/btshare

test:
	go vet ./... && go test ./...

install:
	./install.sh

uninstall:
	./install.sh --uninstall

.PHONY: build test install uninstall
