VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/forkman
MAX_SIZE_BYTES := 13631488   # 13 MiB
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build test vet fmt deps-check release size clean

all: vet test build size

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/forkman

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

deps-check:
	./scripts/check-deps.sh

release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "building dist/forkman_$${os}_$${arch}$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags="$(LDFLAGS)" -o dist/forkman_$${os}_$${arch}$$ext ./cmd/forkman || exit 1; \
	done

size: build
	@n=$$(wc -c < $(BIN)); \
	echo "$(BIN): $$n bytes"; \
	if [ "$$n" -gt $(MAX_SIZE_BYTES) ]; then \
		echo "binary exceeds $(MAX_SIZE_BYTES) bytes"; exit 1; \
	fi

clean:
	rm -rf bin dist
