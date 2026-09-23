# Build/test entry points. The static build flags live here, not in a container
# image (ADR-005) — the binary is the release artifact.

BIN     := bin/verifierd
MXSIM   := bin/mxsim
PKG     := ./...
LDFLAGS := -s -w

.PHONY: all build mxsim test race cover test-redis fuzz fuzz-list vet fmt fmt-check lint gate run clean

all: build

## build: static, dependency-free binary (ADR-005)
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/verifierd

## mxsim: the fake MX used by integration tests
mxsim:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(MXSIM) ./cmd/mxsim

test race:
	go test -race -count=1 $(PKG)

## cover: per-package coverage held to scripts/coverage-floors.txt (plan 027)
cover:
	scripts/coverage-gate.sh

## test-redis: the suite with the real-Redis tests switched on, against a
## throwaway redis:7-alpine named ev-test-redis on 127.0.0.1:56379 (started if
## it is not already running). Point REDIS_TEST_ADDR elsewhere to use your own.
REDIS_TEST_ADDR ?= 127.0.0.1:56379
test-redis:
	@docker inspect -f '{{.State.Running}}' ev-test-redis 2>/dev/null | grep -q true || \
		{ docker run -d --rm --name ev-test-redis -p $(REDIS_TEST_ADDR):6379 redis:7-alpine >/dev/null && sleep 1; }
	VERIFIERD_TEST_REDIS_ADDR=$(REDIS_TEST_ADDR) go test -race -count=1 $(PKG)

## fuzz: each target for FUZZTIME (the weekly CI job runs 60s). -fuzz takes one
## target per package per run, hence the loop. Seeds under testdata/fuzz/ run in
## every plain `go test` regardless.
FUZZTIME ?= 60s
FUZZ_TARGETS := \
	./internal/prober:FuzzReadReply \
	./internal/prober:FuzzClassify \
	./internal/prober:FuzzEnhancedCode \
	./internal/prober:FuzzParseHint \
	./internal/prober:FuzzRedactReply \
	./internal/redis:FuzzRESPReply \
	./internal/redis:FuzzRESPRoundTrip \
	./internal/resolver:FuzzGuardBlocked \
	./internal/pacer:FuzzBandNormalise \
	./internal/relay:FuzzMessageBuild \
	./internal/api:FuzzProbeHandler

fuzz:
	@set -e; for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; \
		echo "== $$fn ($$pkg, $(FUZZTIME))"; \
		go test -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME) $$pkg; \
	done

fuzz-list:
	@for t in $(FUZZ_TARGETS); do echo $$t; done

vet:
	go vet $(PKG)

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint:
	golangci-lint run

## gate: what .github/workflows/ci.yml runs (CLAUDE.md Phase 4). CI also sets
## VERIFIERD_TEST_REDIS_ADDR; `make test-redis` is the local equivalent.
gate: test cover vet fmt-check lint

run:
	go run ./cmd/verifierd -config config/verifierd.yaml

clean:
	rm -rf bin
