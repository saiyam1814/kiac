VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0-dev)
LDFLAGS := -X github.com/saiyam1814/kiac/cmd.Version=$(VERSION)
EDGE_PROXY_BIN := bin/kiac-edge-proxy-linux-arm64
EDGE_PROXY_ASSET := pkg/cluster/assets/kiac-edge-proxy-linux-arm64.gz

.PHONY: build edge-proxy-asset install test lint clean

build: edge-proxy-asset
	go build -ldflags "$(LDFLAGS)" -o bin/kiac .

edge-proxy-asset:
	mkdir -p bin pkg/cluster/assets
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath -ldflags "-s -w" -o $(EDGE_PROXY_BIN) ./cmd/kiac-edge-proxy
	gzip -9nc $(EDGE_PROXY_BIN) > $(EDGE_PROXY_ASSET)

install: edge-proxy-asset
	go install -ldflags "$(LDFLAGS)" .

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -rf bin
