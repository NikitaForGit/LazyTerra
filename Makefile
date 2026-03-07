.PHONY: build install clean uninstall lint test

BINARY  := lazyterra
MODULE  := github.com/NikitaForGit/LazyTerra
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/lazyterra/

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/lazyterra/

clean:
	rm -f $(BINARY)
	rm -rf dist/

uninstall:
	rm -f $(shell go env GOPATH)/bin/$(BINARY)

lint:
	@which golangci-lint > /dev/null || (echo "golangci-lint not found. Install: https://golangci-lint.run/welcome/install/" && exit 1)
	golangci-lint run ./...

test:
	go test -race -v ./...
