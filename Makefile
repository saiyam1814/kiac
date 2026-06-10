VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0-dev)
LDFLAGS := -X github.com/saiyam1814/kiac/cmd.Version=$(VERSION)

.PHONY: build install test lint clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/kiac .

install:
	go install -ldflags "$(LDFLAGS)" .

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -rf bin
