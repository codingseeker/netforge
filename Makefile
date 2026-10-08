GO ?= go
BIN_DIR ?= bin
BINARY := $(BIN_DIR)/netforge
PKG := ./...
COVERAGE := coverage.out
FUZZ_TIME ?= 20s
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# Expanded lazily so it sees the VERSION and COMMIT defaults above.
LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
GOVULNCHECK_VERSION ?= v1.7.0

.DEFAULT_GOAL := check

.PHONY: all
all: check

.PHONY: help
help:
	@echo "NetForge make targets"
	@echo "  build          compile the CLI into $(BINARY)"
	@echo "  version        $(VERSION) ($(COMMIT))"
	@echo "  fmt            format the sources"
	@echo "  fmt-check      fail when sources are not formatted"
	@echo "  vet            run go vet"
	@echo "  test           run unit, integration and security tests"
	@echo "  test-repeat    run the whole suite REPEAT times, for flakiness"
	@echo "  test-unit      run internal package tests only"
	@echo "  test-integration  run tests/integration"
	@echo "  test-security  run tests/security"
	@echo "  test-race      run every test with the race detector"
	@echo "  test-fuzz-seeds  replay the recorded fuzz corpus"
	@echo "  cover          write $(COVERAGE)"
	@echo "  fuzz           fuzz the configuration parsers for $(FUZZ_TIME)"
	@echo "  fuzz-packet    fuzz the packet parsers and checksum for $(FUZZ_TIME)"
	@echo "  fuzz-router    fuzz the forwarding decision for $(FUZZ_TIME)"
	@echo "  vuln           run govulncheck against the module"
	@echo "  tidy           tidy go.mod and go.sum"
	@echo "  check          fmt-check vet test race cover build"
	@echo "  clean          remove build and coverage artefacts"

.PHONY: build
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/netforge

.PHONY: fmt
fmt:
	$(GO) fmt $(PKG)

.PHONY: fmt-check
fmt-check:
	@files=$$(gofmt -l cmd internal tests); \
	if [ -n "$$files" ]; then \
		echo "gofmt required for:"; echo "$$files"; exit 1; \
	fi

.PHONY: vet
vet:
	$(GO) vet $(PKG)

.PHONY: test
test:
	$(GO) test -count=1 $(PKG)

# A suite that passes once may still be flaky, and a flaky suite makes every
# other gate untrustworthy. REPEAT runs the whole thing end to end so a test
# that passes by luck is caught rather than reported as evidence.
REPEAT ?= 3

.PHONY: test-repeat
test-repeat:
	@for round in $$(seq 1 $(REPEAT)); do \
		echo "=== pass $$round of $(REPEAT) ==="; \
		$(GO) test -count=1 $(PKG) || exit 1; \
	done

.PHONY: test-unit
test-unit:
	$(GO) test -count=1 ./internal/...

.PHONY: test-integration
test-integration:
	$(GO) test -count=1 -v ./tests/integration/...

.PHONY: test-security
test-security:
	$(GO) test -count=1 -v ./tests/security/...

.PHONY: test-race
test-race:
	$(GO) test -count=1 -race $(PKG)

.PHONY: cover
cover:
	$(GO) test -count=1 -covermode=atomic -coverpkg=./... -coverprofile=$(COVERAGE) $(PKG)
	$(GO) tool cover -func=$(COVERAGE) | tail -1

.PHONY: fuzz
fuzz:
	$(GO) test -run '^$$' -fuzz '^FuzzParse$$' -fuzztime=$(FUZZ_TIME) ./internal/config/
	$(GO) test -run '^$$' -fuzz '^FuzzParseBandwidth$$' -fuzztime=$(FUZZ_TIME) ./internal/config/
	$(GO) test -run '^$$' -fuzz '^FuzzParseDuration$$' -fuzztime=$(FUZZ_TIME) ./internal/config/
	$(GO) test -run '^$$' -fuzz '^FuzzParsePercent$$' -fuzztime=$(FUZZ_TIME) ./internal/config/
	$(GO) test -run '^$$' -fuzz '^FuzzValidateName$$' -fuzztime=$(FUZZ_TIME) ./internal/config/

# Each packet fuzz target is run on its own. Go allows one -fuzz pattern per
# invocation, and a target that stops early would hide the ones after it.
PACKET_FUZZ_TARGETS := FuzzParseEthernet FuzzParseIPv4 FuzzParseICMP FuzzParseUDP FuzzParseTCP FuzzParseFrame FuzzChecksum FuzzRefreshTransportChecksum

ROUTER_FUZZ_TARGETS := FuzzDecide FuzzICMPError

.PHONY: fuzz-packet
fuzz-packet:
	@for target in $(PACKET_FUZZ_TARGETS); do \
		echo "fuzzing $$target for $(FUZZ_TIME)"; \
		$(GO) test -run '^$$' -fuzz "^$$target$$" -fuzztime=$(FUZZ_TIME) ./internal/packet/ || exit 1; \
	done

# The forwarding decision turns untrusted frames into actions, so it gets its
# own target. Only the decision is fuzzed: it needs no interface.
.PHONY: fuzz-router
fuzz-router:
	@for target in $(ROUTER_FUZZ_TARGETS); do \
		echo "fuzzing $$target for $(FUZZ_TIME)"; \
		$(GO) test -run '^$$' -fuzz "^$$target$$" -fuzztime=$(FUZZ_TIME) ./internal/router/ || exit 1; \
	done

# The fuzz corpus is replayed as ordinary tests, so the seeds in testdata/fuzz
# are covered by every test run and not only by a fuzzing run.
.PHONY: test-fuzz-seeds
test-fuzz-seeds:
	$(GO) test -count=1 -run 'Fuzz' ./internal/packet/ ./internal/config/ ./internal/router/

.PHONY: vuln
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: check
check: fmt-check vet test test-race cover build

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) $(COVERAGE)
	$(GO) clean -testcache
