VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0-dev)
LDFLAGS := -X github.com/saiyam1814/kiac/cmd.Version=$(VERSION)
EDGE_PROXY_BIN := bin/kiac-edge-proxy-linux-arm64
EDGE_PROXY_ASSET := pkg/cluster/assets/kiac-edge-proxy-linux-arm64.gz
EDGE_PROXY_LDFLAGS := -s -w -buildid=
ASSET_GZIP := go run ./internal/cmd/asset-gzip

.PHONY: build edge-proxy-asset edge-proxy-check install test test-race lint fmt-check tidy-check ci runtime-smoke clean

build: edge-proxy-asset
	go build -ldflags "$(LDFLAGS)" -o bin/kiac .

edge-proxy-asset:
	mkdir -p bin pkg/cluster/assets
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath -ldflags "$(EDGE_PROXY_LDFLAGS)" -o $(EDGE_PROXY_BIN) ./cmd/kiac-edge-proxy
	$(ASSET_GZIP) $(EDGE_PROXY_BIN) $(EDGE_PROXY_ASSET)

edge-proxy-check:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath -ldflags "$(EDGE_PROXY_LDFLAGS)" -o "$$tmp/kiac-edge-proxy" ./cmd/kiac-edge-proxy; \
	$(ASSET_GZIP) "$$tmp/kiac-edge-proxy" "$$tmp/kiac-edge-proxy.gz"; \
	cmp -s "$$tmp/kiac-edge-proxy.gz" $(EDGE_PROXY_ASSET) || { \
		echo "$(EDGE_PROXY_ASSET) is stale; run: make edge-proxy-asset"; exit 1; \
	}

install: edge-proxy-asset
	go install -ldflags "$(LDFLAGS)" .

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	go vet ./...

fmt-check:
	@files="$$(gofmt -l $$(git ls-files --cached --others --exclude-standard -- '*.go'))"; \
	if [ -n "$$files" ]; then printf 'gofmt required:\n%s\n' "$$files"; exit 1; fi

tidy-check:
	@before="$$(cksum go.mod go.sum)"; \
	go mod tidy; \
	after="$$(cksum go.mod go.sum)"; \
	if [ "$$before" != "$$after" ]; then echo "go.mod or go.sum was not tidy"; exit 1; fi

ci: fmt-check tidy-check edge-proxy-check lint test-race
	go build ./...

runtime-smoke: build
	./test/e2e/run.sh "$${PROFILE:-quick}"

clean:
	rm -rf bin
