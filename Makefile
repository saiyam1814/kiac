VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0-dev)
LDFLAGS := -X github.com/saiyam1814/kiac/cmd.Version=$(VERSION)
EDGE_PROXY_BIN := bin/kiac-edge-proxy-linux-arm64
EDGE_PROXY_ASSET := pkg/cluster/assets/kiac-edge-proxy-linux-arm64.gz
EDGE_PROXY_LDFLAGS := -s -w -buildid=
GPU_AGENT_BIN := bin/kiac-gpu-agent-linux-arm64
GPU_AGENT_ASSET := pkg/cluster/assets/kiac-gpu-agent-linux-arm64.gz
GPU_AGENT_LDFLAGS := -s -w -buildid=
ASSET_GZIP := go run ./internal/cmd/asset-gzip

.PHONY: build edge-proxy-asset edge-proxy-check gpu-agent-asset gpu-agent-check gpu-agent-test gpu-agent-tidy-check install test test-race lint fmt-check tidy-check ci runtime-smoke clean

build: edge-proxy-asset gpu-agent-asset
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

gpu-agent-asset:
	mkdir -p bin pkg/cluster/assets
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C internal/gpudriver build -buildvcs=false -trimpath -ldflags "$(GPU_AGENT_LDFLAGS)" -o ../../$(GPU_AGENT_BIN) .
	$(ASSET_GZIP) $(GPU_AGENT_BIN) $(GPU_AGENT_ASSET)

gpu-agent-check:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C internal/gpudriver build -buildvcs=false -trimpath -ldflags "$(GPU_AGENT_LDFLAGS)" -o "$$tmp/kiac-gpu-agent" .; \
	$(ASSET_GZIP) "$$tmp/kiac-gpu-agent" "$$tmp/kiac-gpu-agent.gz"; \
	cmp -s "$$tmp/kiac-gpu-agent.gz" $(GPU_AGENT_ASSET) || { \
		echo "$(GPU_AGENT_ASSET) is stale; run: make gpu-agent-asset"; exit 1; \
	}

gpu-agent-test:
	go -C internal/gpudriver test ./...
	go -C internal/gpudriver vet ./...

gpu-agent-tidy-check:
	@before="$$(cksum internal/gpudriver/go.mod internal/gpudriver/go.sum)"; \
	go -C internal/gpudriver mod tidy; \
	after="$$(cksum internal/gpudriver/go.mod internal/gpudriver/go.sum)"; \
	if [ "$$before" != "$$after" ]; then echo "internal/gpudriver/go.mod or go.sum was not tidy"; exit 1; fi

install: edge-proxy-asset gpu-agent-asset
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

ci: fmt-check tidy-check gpu-agent-tidy-check edge-proxy-check gpu-agent-check gpu-agent-test lint test-race
	go build ./...

runtime-smoke: build
	./test/e2e/run.sh "$${PROFILE:-quick}"

clean:
	rm -rf bin
