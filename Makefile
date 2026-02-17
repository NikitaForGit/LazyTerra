.PHONY: build install clean

BINARY := lazyterra
MODULE := github.com/lazyterra/lazyterra

build:
	go build -o $(BINARY) ./cmd/lazyterra/

install:
	go install ./cmd/lazyterra/

clean:
	rm -f $(BINARY)

lint:
	golangci-lint run ./...

test:
	go test ./...
