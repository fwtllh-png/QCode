GO ?= go
NPM ?= npm
BINARY := bin/qcode
MODULE := github.com/fwtllh-png/QCode
START_WORKSPACE ?= $(CURDIR)
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
INSTALL_BINARY := $(BINDIR)/qcode
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.Date=$(BUILD_DATE)
WEB_BUILD_TAG := webbundle

.PHONY: start install uninstall fmt verify test test-hermetic test-platform-capability reliability-gate test-integration \
	test-release release-baseline-check integration-gate release-gate race build cross-build smoke \
	capacity-policy-check \
	security-side-effect-check \
	docs-check book-check web-experience-check \
	host-journey-contract \
	benchmark-v2-check benchmark-v2 hotspot-baseline \
	web-protocol web-protocol-check \
	provider-deepseek-live-control provider-deepseek-live-ce7 \
	architecture-freeze \
	book-navigation \
	turn-kernel-convergence-baseline turn-kernel-convergence-exit-gate \
	doc-governance-check doc-governance-test doc-impact \
	doc-reverify doc-reverify-dry-run \
	doc-external-links release-fact-check brand-check \
	security-test sandbox-attack-test secret-leak-test \
	stress stress-nightly \
	web-host-smoke protocol-contract protocol-schema \
	web-install web-check web-test web-compile web-measure web-build web-brand-assets web-assets-check web-e2e \
		web-release-drill web-streaming-soak web-performance web-supply-chain-check web-vulnerability-check \
	bench catalog-bench package clean

# Stress tests run concurrent pressure tests to catch deadlocks,
# goroutine leaks, and channel blocking under high concurrency.
# Run with: make stress
stress:
	$(GO) test -tags=stress -race -count=1 -timeout 5m -run '^TestStress' \
		./internal/runtime/agent/engine/... \
		./internal/adapter/mcp/... \
		./internal/runtime/app/eventhub/...

# stress-nightly runs extended stress tests for manual overnight validation.
stress-nightly:
	$(GO) test -tags=stress -race -count=3 -timeout 15m -run '^TestStress' \
		./internal/runtime/agent/engine/... \
		./internal/adapter/mcp/... \
		./internal/runtime/app/eventhub/...

PROTOCOL_SCHEMA := docs/protocol/runtime-protocol.schema.json
WEB_HOST_CONTRACT := docs/protocol/web-host.contract.json
WEB_HOST_TYPES := web/src/protocol/web-host.generated.ts
WEB_HOST_ROUTES_GO := internal/host/runtimeapi/web/unary_routes.generated.go
WEB_STREAMING_SOAK_DURATION ?= 1h
WEB_STREAMING_SOAK_TIMEOUT ?= 70m
WEB_STREAMING_SOAK_ALLOW_SHORT ?= 0
RELIABILITY_MATRIX := testdata/contracts/reliability-matrix.json
WEB_MEASUREMENT_REPORT ?= .tmp/web-supply-chain-report.json
WEB_INSTALL_STAMP := web/node_modules/.package-lock.json
BASE_REF ?= origin/main

RELEASE_STAGE ?= experimental
PREVIOUS_RELEASE_REF ?=
PREVIOUS_BINARY ?=
TEST_LANE_REPORT_DIR ?= .tmp/test-lanes
TEST_PACKAGE_PARALLELISM ?= 1
TEST_HOME ?= $(CURDIR)/.tmp/test-home
TEST_GOPATH ?= $(shell $(GO) env GOPATH)
TEST_GOMODCACHE ?= $(shell $(GO) env GOMODCACHE)
TEST_GOCACHE ?= $(shell $(GO) env GOCACHE)
TEST_HOME_ENV := HOME='$(TEST_HOME)' GOPATH='$(TEST_GOPATH)' \
	GOMODCACHE='$(TEST_GOMODCACHE)' GOCACHE='$(TEST_GOCACHE)'
PLATFORM_CAPABILITY_ARGS := --available-on darwin --available-on linux

ifeq ($(shell uname -s 2>/dev/null),Darwin)
PLATFORM_CAPABILITY_ARGS += --requires-command sandbox-exec
else ifeq ($(shell uname -s 2>/dev/null),Linux)
PLATFORM_CAPABILITY_ARGS += --requires-command bwrap
endif

fmt:
	$(GO) fmt ./...

capacity-policy-check:
	$(GO) test ./scripts -run '^TestCapacityPathsDoNotReintroduceLegacyTiers$$'

verify: docs-check book-check brand-check web-protocol-check \
	web-check web-test web-assets-check web-supply-chain-check \
	reliability-gate
	@unformatted="$$(git ls-files --cached --others --exclude-standard '*.go' | \
		while IFS= read -r file; do \
			test ! -f "$$file" || gofmt -l "$$file"; \
		done)"; \
		test -z "$$unformatted" || { \
			echo "gofmt required:"; printf '%s\n' "$$unformatted"; exit 1; \
		}
	$(GO) vet ./...
	$(MAKE) test-hermetic
	$(GO) test -race -p 1 ./...

test: test-hermetic

hotspot-baseline:
	$(GO) test -count=1 ./scripts -run 'Test(RepositoryHotspotBaseline|CheckHotspot)'
	$(GO) run ./scripts/check-hotspot-baseline.go -root .

provider-deepseek-live-control:
	QCODE_DEEPSEEK_LIVE_CONTROL=1 \
		$(GO) test -count=1 -v ./internal/adapter/provider/httpclient \
		-run '^TestDeepSeekP0LiveControl$$'

provider-deepseek-live-ce7:
	QCODE_DEEPSEEK_LIVE_CONTROL=1 \
		$(GO) test -count=1 -v ./internal/adapter/provider/httpclient \
		-run '^TestDeepSeek(P0LiveControl|CE7LiveCacheShare)$$'

# Architecture behavior freeze. Package tests carry characterization, config
# provenance drift, state transitions, and schema drift. Race is focused on the
# concurrent turn engine.
architecture-freeze: hotspot-baseline
	@mkdir -p '$(TEST_HOME)'
	$(TEST_HOME_ENV) $(GO) test -count=1 \
		./internal/runtime/agent/engine \
		./internal/config \
		./internal/runtime/protocol
	$(TEST_HOME_ENV) $(GO) test -race -count=1 \
		./internal/runtime/agent/engine

# Hermetic is the default developer lane: no network, live credentials, GUI,
# or host sandbox capability. Serial package execution avoids resource flakes.
test-hermetic:
	@mkdir -p '$(TEST_HOME)'
	python3 scripts/run-test-lane.py hermetic \
		--report '$(TEST_LANE_REPORT_DIR)/hermetic.json' \
		-- env $(TEST_HOME_ENV) $(GO) test -count=1 \
			-p '$(TEST_PACKAGE_PARALLELISM)' ./...

# Capability tests are compiled only in this lane. Missing host prerequisites
# produce an explicit unavailable report; callers may set CAPABILITY_REQUIRED.
test-platform-capability:
	QCODE_SANDBOX_STAGE=1 python3 scripts/run-test-lane.py platform-capability \
		--report '$(TEST_LANE_REPORT_DIR)/platform-capability.json' \
		--unavailable-pattern sandbox_unavailable \
		$(PLATFORM_CAPABILITY_ARGS) $(CAPABILITY_REQUIRED) \
		-- $(GO) test -tags=capability -count=1 \
			./internal/security/sandbox/... ./internal/platform/process/...

reliability-gate:
	python3 scripts/check-reliability-matrix.py \
		'$(RELIABILITY_MATRIX)' --run

test-integration:
	python3 scripts/run-test-lane.py integration \
		--report '$(TEST_LANE_REPORT_DIR)/integration.json' \
		--requires-command go --requires-command npm \
		$(INTEGRATION_REQUIRED) \
		-- $(MAKE) integration-gate

integration-gate: build
	$(GO) test -count=1 ./internal/host/runtimeapi/web ./internal/host/web

test-release: release-baseline-check
	python3 scripts/run-test-lane.py release \
		--report '$(TEST_LANE_REPORT_DIR)/release.json' \
		--requires-command go --requires-command npm \
		--unavailable-pattern sandbox_unavailable \
		--require-available \
		-- $(MAKE) release-gate

release-baseline-check:
	@if test -n '$(PREVIOUS_BINARY)'; then \
		test -x '$(PREVIOUS_BINARY)' || { \
			echo "PREVIOUS_BINARY is not executable: $(PREVIOUS_BINARY)" >&2; \
			exit 2; \
		}; \
	else \
		./scripts/validate-release-ref.sh '$(PREVIOUS_RELEASE_REF)' >/dev/null; \
	fi

release-gate: cross-build smoke race secret-leak-test reliability-gate benchmark-v2 web-performance \
	web-streaming-soak web-release-drill web-supply-chain-check web-vulnerability-check
	@dirty="$$(git status --porcelain --untracked-files=all)"; \
		test -z "$$dirty" || { \
			echo "release gate requires a clean worktree:"; \
			printf '%s\n' "$$dirty"; \
			exit 1; \
		}

race:
	$(GO) test -race -p 1 ./...

build: web-build
	@mkdir -p bin
	$(GO) build -tags '$(WEB_BUILD_TAG)' -trimpath \
		-ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/qcode

start:
	$(MAKE) web-install
	$(MAKE) build
	./$(BINARY) --workspace '$(START_WORKSPACE)' --enable-tools --posture suggest --replace-owner --open

install:
	$(MAKE) web-install
	$(MAKE) build
	@mkdir -p '$(BINDIR)'
	@tmp="$$(mktemp '$(BINDIR)/.qcode.XXXXXX')"; \
		trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
		cp '$(BINARY)' "$$tmp"; \
		chmod 0755 "$$tmp"; \
		mv -f "$$tmp" '$(INSTALL_BINARY)'
	@printf 'Installed QCode: %s\n' '$(INSTALL_BINARY)'
	@if command -v qcode >/dev/null 2>&1; then \
		printf 'Run from any workspace: qcode\n'; \
	else \
		printf 'Add this directory to PATH: export PATH="%s:$$PATH"\n' '$(BINDIR)'; \
	fi

uninstall:
	@rm -f '$(INSTALL_BINARY)'
	@printf 'Removed QCode: %s\n' '$(INSTALL_BINARY)'

web-install: $(WEB_INSTALL_STAMP)

$(WEB_INSTALL_STAMP): web/package.json web/package-lock.json
	$(NPM) --prefix web ci
	@test -f '$(WEB_INSTALL_STAMP)'

web-check:
	$(NPM) --prefix web run check

web-test:
	$(NPM) --prefix web test

web-performance:
	$(NPM) --prefix web test -- --run src/ui/performance.test.ts

web-supply-chain-check: web-build

web-vulnerability-check:
	@mkdir -p .tmp
	@$(NPM) --prefix web audit --audit-level=high --json > .tmp/web-npm-audit.json || { \
		cat .tmp/web-npm-audit.json; \
		exit 1; \
	}

web-compile: $(WEB_INSTALL_STAMP)
	$(NPM) --prefix web run build
	$(GO) run ./scripts/webassetmanifest -dist web/dist -output web/dist/asset-manifest.json

web-measure: web-compile
	node scripts/web-supply-chain-check.mjs . --measure-only \
		--report '$(WEB_MEASUREMENT_REPORT)'

web-build: web-compile
	node scripts/web-supply-chain-check.mjs .

web-brand-assets:
	$(NPM) --prefix web run brand-assets

web-assets-check: web-brand-assets web-build
	@tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	$(NPM) --prefix web run build -- --outDir "$$tmp/dist" >/dev/null; \
	$(GO) run ./scripts/webassetmanifest \
		-dist "$$tmp/dist" -output "$$tmp/dist/asset-manifest.json"; \
	diff -ru web/dist "$$tmp/dist"

web-e2e: web-assets-check build
	$(NPM) --prefix web run test:e2e

web-release-drill: build
	@tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	previous='$(PREVIOUS_BINARY)'; \
	if test -z "$$previous"; then \
		test -n '$(PREVIOUS_RELEASE_REF)' || { \
			echo "PREVIOUS_RELEASE_REF or PREVIOUS_BINARY is required" >&2; \
			exit 2; \
		}; \
		./scripts/validate-release-ref.sh '$(PREVIOUS_RELEASE_REF)' >/dev/null; \
		git archive '$(PREVIOUS_RELEASE_REF)' | tar -x -C "$$tmp"; \
		(cd "$$tmp" && \
			$(NPM) --prefix web ci >/dev/null && \
			$(MAKE) build BINARY="$$tmp/qcode-previous"); \
		previous="$$tmp/qcode-previous"; \
	fi; \
	python3 scripts/web-release-drill.py \
		--current-binary '$(CURDIR)/$(BINARY)' \
		--previous-binary "$$previous" \
		--workspace '$(CURDIR)' \
		--fixture '$(CURDIR)/testdata/providers/openai' \
		--report '$(CURDIR)/.tmp/release/web-downgrade-drill.json'

web-streaming-soak:
	QCODE_WEB_STREAMING_SOAK_DURATION=$(WEB_STREAMING_SOAK_DURATION) \
		QCODE_WEB_STREAMING_SOAK_ALLOW_SHORT=$(WEB_STREAMING_SOAK_ALLOW_SHORT) \
		$(GO) test -count=1 -timeout $(WEB_STREAMING_SOAK_TIMEOUT) \
		-run '^TestWebSocketSustainedStreamingSoak$$' \
		./internal/host/runtimeapi/web

cross-build: web-build
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -tags '$(WEB_BUILD_TAG)' -trimpath -o "$$tmp/qcode-linux-amd64" ./cmd/qcode; \
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -tags '$(WEB_BUILD_TAG)' -trimpath -o "$$tmp/qcode-linux-arm64" ./cmd/qcode; \
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -tags '$(WEB_BUILD_TAG)' -trimpath -o "$$tmp/qcode-windows-amd64.exe" ./cmd/qcode

smoke: build
	./$(BINARY) --help >/dev/null
	./$(BINARY) --version

docs-check: web-experience-check benchmark-v2-check catalog-check
	./scripts/check-docs.sh
	$(MAKE) doc-governance-check
	$(MAKE) doc-governance-test

book-check:
	./scripts/check-book.sh

book-navigation:
	python3 scripts/render-book-navigation.py

turn-kernel-convergence-baseline:
	$(GO) test -count=1 \
		./internal/runtime/agent/turnkernel \
		./internal/runtime/agent/engine \
		./internal/runtime/app \
		./internal/runtime/app/wire \
		./internal/persist/state/sqlite \
		./internal/persist/state/turnstate \
		-run 'Test(C0|C1|C2|C3|C4|C5|C6|Phase4R)'

# Final production ownership gate.
turn-kernel-convergence-exit-gate:
	QCODE_TURN_KERNEL_CONVERGENCE_EXIT_GATE=1 $(GO) test -count=1 \
		./internal/runtime/agent/turnkernel \
		./internal/runtime/app \
		-run '^TestC0.*ExitGate$$'

web-experience-check:
	$(GO) run ./scripts/webexperiencecheck

host-journey-contract:
	$(GO) test -count=1 ./internal/host/runtimeapi/runtimecontract
	$(GO) test -count=1 ./internal/host/runtimeapi/web
	$(GO) test -count=1 ./internal/host/web
	$(NPM) --prefix web test

doc-governance-check:
	python3 scripts/check-doc-governance.py check

doc-governance-test:
	python3 -m unittest discover -s scripts/tests -p 'test_*.py'

doc-reverify:
	python3 scripts/check-doc-governance.py reverify

doc-reverify-dry-run:
	python3 scripts/check-doc-governance.py reverify --dry-run

doc-impact:
	@test -n "$(BASE_REF)" || { echo "BASE_REF is required" >&2; exit 2; }
	python3 scripts/check-doc-governance.py impact --base "$(BASE_REF)" --head "$${HEAD_REF:-HEAD}"

doc-external-links:
	python3 scripts/check-doc-governance.py external-links

release-fact-check:
	python3 scripts/check-doc-governance.py release

brand-check:
	./scripts/check-brand.sh
	./scripts/test-brand-check.sh

security-test:
	$(MAKE) security-side-effect-check
	$(GO) test -race ./internal/security/... ./internal/adapter/mcp/... ./internal/adapter/tool/guard/... ./internal/adapter/tool/quality/... ./internal/host/web/... ./internal/runtime/agent/engine/... ./internal/runtime/app/...
	$(GO) test -race ./internal/platform/process/... -run 'Test(RunUsesInjectedStrongSandboxBackend|RunFailsClosedWithoutStrongSandbox|RunSanitizesRegularAndPTYEnvironments|RunPinsWorkingDirectoryToDescriptor|SanitizedEnvironment)'

sandbox-attack-test:
	QCODE_SANDBOX_STAGE=1 $(GO) test -tags=capability -race \
		./internal/security/sandbox/... ./internal/adapter/tool/file/... ./internal/adapter/tool/shell/...
	QCODE_SANDBOX_STAGE=1 $(GO) test -tags=capability -race \
		./internal/platform/process/... \
		-run 'Test(RunUsesInjectedStrongSandboxBackend|RunFailsClosedWithoutStrongSandbox|RunPinsWorkingDirectoryToDescriptor|SessionCancellationKillsProcessGroup|RealSandboxAttackCorpus|RealManagedProxyBlocksDirectEgress)'

secret-leak-test: build
	$(GO) test -race ./internal/config ./internal/observability/telemetry \
		./internal/platform/process/... \
		-run 'Test(Secret|JSONLogger|RunSanitizesRegularAndPTYEnvironments|SanitizedEnvironment)'

security-side-effect-check:
	$(GO) test ./scripts/securityeffects
	$(GO) run ./scripts/securityeffects -root .

web-host-smoke:
	$(GO) test -race -count=1 ./internal/host/web/...

protocol-contract:
	$(GO) test -count=1 -v ./internal/runtime/app/... ./internal/host/runtimeapi/web/...

# protocol-schema regenerates the published protocol shapes. The drift test in
# internal/runtime/protocol fails when the committed copy is stale.
protocol-schema:
	$(GO) run ./scripts/eventtraitgen ./internal/runtime/protocol/event_traits.json ./internal/runtime/protocol/event_traits.gen.go
	$(GO) run ./internal/runtime/protocol/schemagen $(PROTOCOL_SCHEMA)
	$(GO) run ./scripts/webprotocolgen -output $(WEB_HOST_CONTRACT) \
		-typescript $(WEB_HOST_TYPES) -go-output $(WEB_HOST_ROUTES_GO)

web-protocol:
	$(GO) run ./scripts/webprotocolgen -output $(WEB_HOST_CONTRACT) \
		-typescript $(WEB_HOST_TYPES) -go-output $(WEB_HOST_ROUTES_GO)

web-protocol-check:
	$(GO) run ./scripts/webprotocolgen -output $(WEB_HOST_CONTRACT) \
		-typescript $(WEB_HOST_TYPES) -go-output $(WEB_HOST_ROUTES_GO) -check

# bench runs the hermetic coding benchmark (fixture provider, no network/model).
# Set BENCH_REPORT to write the JSON report for tracking across runs.
bench:
	QCODE_BENCH_REPORT='$(BENCH_REPORT)' $(GO) test -tags=capability \
		-count=1 -v ./internal/host/bench/...

benchmark-v2-check:
	$(GO) test -count=1 ./scripts/benchmarkv2
	$(GO) run ./scripts/benchmarkv2 -root .

# catalog-check keeps the hand-maintained bundled model catalog honest:
# pricing, limit relationships, provenance vocabulary, and cross-model
# consistency, on top of the runtime per-provider validation.
catalog-check:
	$(GO) test -count=1 ./scripts/catalogcheck
	$(GO) run ./scripts/catalogcheck -root .

benchmark-v2: benchmark-v2-check bench
	$(GO) test -count=1 -run 'Recovery' ./internal/persist/workspacejournal
	$(GO) test -count=1 -run '^TestWebSocketDownlinkConcurrencyAndShutdown$$' \
		./internal/host/runtimeapi/web
	$(GO) test -count=1 -run '^TestRunContextStartsAndStopsWebHost$$' \
		./internal/host/web
	$(GO) test -count=1 -run '^TestWeb(Socket(ReplaysTenThousandEvents|CapsBrowserConnectionsAtSixteen|DisconnectStormReleasesSlotsGoroutinesAndDescriptors)|SessionCapacity(AllowsThirtyTwoAndPreservesIdempotentRetry|IsAtomicUnderConcurrentCreate))$$' \
		./internal/host/runtimeapi/web
	$(NPM) --prefix web run test:e2e -- visual.spec.ts --grep 'reloads|frozen'
	$(NPM) --prefix web test -- --testNamePattern \
		'windows 500-turn transcripts to 200 projected rows with older and newer navigation'

# catalog-bench tracks the M4 dynamic tool catalog's time, allocation, and
# prompt-size baseline at 100/500/1000 tools.
catalog-bench:
	$(GO) test -run '^$$' \
		-bench 'BenchmarkTool(Catalog|RegistryStartup)Scale' \
		-benchtime=10x -benchmem ./internal/runtime/agent/prompt

package: web-assets-check build
	VERSION='$(VERSION)' RELEASE_STAGE='$(RELEASE_STAGE)' ./scripts/package-release.sh

clean:
	rm -rf bin dist .tmp .dbg web/dist
