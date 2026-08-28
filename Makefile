# Build/test entry points. The static build flags live here, not in a container
# image (ADR-005) — the binary is the release artifact.

BIN     := bin/verifierd
MXSIM   := bin/mxsim
PKG     := ./...
LDFLAGS := -s -w

.PHONY: all build mxsim test race vet fmt fmt-check lint gate run clean

all: build

## build: static, dependency-free binary (ADR-005)
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/verifierd

## mxsim: the fake MX used by integration tests
mxsim:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(MXSIM) ./cmd/mxsim

test race:
	go test -race -count=1 $(PKG)

vet:
	go vet $(PKG)

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint:
	golangci-lint run

## gate: exactly what .github/workflows/ci.yml runs (CLAUDE.md Phase 4)
gate: test vet fmt-check lint

run:
	go run ./cmd/verifierd -config config/verifierd.yaml

clean:
	rm -rf bin
