.PHONY: test test-race vet fmt fmt-check test-full test-ui test-web gate dev

# A bare `make` runs the Go tests, as it did before the progress runner.
.DEFAULT_GOAL := test

# Go tests run through scripts/gotestprogress, which turns `go test -json` into progress: each package's start and end with a count, every
# failing test with its output, and, when nothing happens for a while, which tests are running and for how long (see CONTRIBUTING.md,
# "Reading the test output"). It is built into tmp/ and rebuilt when its source changes. Its exit status is go test's.
# PLAIN=1 runs plain go test instead; PROGRESS_FLAGS passes flags to it (for example PROGRESS_FLAGS='-quiet 10s -hang 2m').
GOTESTPROGRESS := tmp/gotestprogress
PROGRESS_FLAGS ?=
# Go's own limit for one package's test binary is 10 minutes, which a slow runner (CI's two CPUs) can pass under -race without any test being
# wrong; the progress runner's HANG? line, not this limit, is what finds a stuck test. GO_TEST_TIMEOUT=10m goes back to Go's default.
GO_TEST_TIMEOUT ?= 25m
# The number of CPUs, for the defaults below. It is worked out with getconf (macOS and Linux), then sysctl, then 1, and never with node: a make
# call evaluates it before anything runs, and test-full has to work on a machine that has no node (the web tests run in Docker). Anything that is
# not a whole number counts as 1. It is worked out once, here, before GO_TEST_SHARDS exists: GNU make expands every exported variable to build the
# environment of a $(shell) call, so an exported default that used a lazily evaluated NCPU would be expanded while NCPU is still being worked out.
ifndef NCPU
NCPU := $(shell n=$$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null); [ "$$n" -ge 1 ] 2>/dev/null || n=1; echo $$n)
endif
# The big packages are split over several go test processes that run side by side, so one package is no longer the whole run's length: the
# tests of a package otherwise run mostly one after the other. GO_TEST_SHARDS=package=count,... picks them (see scripts/gotestprogress and
# scripts/shard-weights.json); the default is for a machine with CPUs to spare and is empty below 8 of them, where more processes only compete.
# GO_TEST_SHARDS= (empty) runs every package in one piece, as CI does.
GO_TEST_SHARDS ?= $(shell [ $(NCPU) -ge 8 ] && echo internal/server=4,internal/store=3,cmd/aimebu=2)
export GO_TEST_SHARDS

$(GOTESTPROGRESS): $(wildcard scripts/gotestprogress/*.go) go.mod
	@mkdir -p tmp
	go build -o $@ ./scripts/gotestprogress

# gotest runs go test with the given arguments, through the progress runner unless PLAIN=1.
define gotest
	@if [ -n "$(PLAIN)" ]; then go test -timeout $(GO_TEST_TIMEOUT) $(1); else $(GOTESTPROGRESS) $(PROGRESS_FLAGS) -- -timeout $(GO_TEST_TIMEOUT) $(1); fi
endef

test: $(GOTESTPROGRESS) ## Run unit tests
	$(call gotest,./...)

# CI's two CPUs can take over 25m for internal/server under -race.
test-race: GO_TEST_TIMEOUT = 45m
test-race: $(GOTESTPROGRESS) ## Run tests with the race detector
	$(call gotest,-race ./...)

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go source
	gofmt -w cmd internal scripts

fmt-check: ## Fail if Go source is unformatted
	@test -z "$$(gofmt -l cmd internal scripts)" || { gofmt -l cmd internal scripts; exit 1; }

test-full: fmt-check vet test-race test-web ## Full local and CI gate

WEB_TESTS := run-jobs server-start api-network chat-model room-panel-model sidebar-model claude-sessions claude-session-import system-events limit-guard sidebar-order preferences expiry room-unread room-notify inbox-notifications harness-offer notification-banner humanize time emoji highlight preview svg-headers macro-import settings-backup composer-markdown composer-paste markdown import-tree quick-commands message-usage role-defaults harness-model prompts-model macros-model roles-model general-model notifications-model usages-model quota-model users-model machines-model project-model shell-model health-model knowledge-model inbox-model agents-model members-model sender-profile-model preact-vendor spawn-model harness-version workspace-folder build-version import-problems signin-model setup-model onboarding-model topbar-model telemetry telemetry-consent

# The web tests are plain node scripts, with no npm dependencies. By default they run in Docker, from a digest-pinned image, so that every
# machine runs the same toolchain: the repository is mounted read-only at /src, copied to a tmpfs, and run from there with no network
# (scripts/test-web-in-container.sh). WEB_TEST_RUNNER=local runs them with the node on PATH instead; there is no automatic fallback either way.
# The pin must pass scripts/check-node-ca.cjs (no compromised CA roots (2026-10) in node's bundled or the system store): `make test-web` runs it first.
WEB_TEST_NODE_IMAGE ?= node:24.22.1-alpine3.24@sha256:06d7adf3e0ef61a13f1d28682726366f97a15aa1888c934d3ecec86c33918b53
WEB_TEST_RUNNER ?= docker
# How many of the test-ui scripts run at once. 1 is the slowest and the quietest; the default is a quarter of the CPUs, at most 4.
UI_JOBS ?= $(shell j=$$(( $(NCPU) / 4 )); [ $$j -lt 1 ] && j=1; [ $$j -gt 4 ] && j=4; echo $$j)

test-web: ## Run the browser-free web unit tests (vm over the module source), in Docker (WEB_TEST_RUNNER=local for local node)
	@set -e; \
	case "$(WEB_TEST_RUNNER)" in \
	local) \
		command -v node >/dev/null 2>&1 || { echo "make test-web: WEB_TEST_RUNNER=local, but node is not on PATH: install node, or drop WEB_TEST_RUNNER to run in Docker" >&2; exit 1; }; \
		echo "make test-web: running with local node $$(node --version)"; \
		sh scripts/test-web.sh $(WEB_TESTS) ;; \
	docker) \
		command -v docker >/dev/null 2>&1 || { echo "make test-web: docker is not on PATH: install Docker, or run with local node: make test-web WEB_TEST_RUNNER=local" >&2; exit 1; }; \
		docker info >/dev/null 2>&1 || { echo "make test-web: the Docker daemon is not reachable: start Docker, or run with local node: make test-web WEB_TEST_RUNNER=local" >&2; exit 1; }; \
		echo "make test-web: running in Docker ($(WEB_TEST_NODE_IMAGE))"; \
		docker run --rm --network none --read-only --tmpfs /tmp --tmpfs /work:rw,size=512m,mode=1777 --cap-drop ALL --security-opt no-new-privileges \
			-u "$$(id -u):$$(id -g)" -e HOME=/tmp -e TMPDIR=/tmp \
			--mount "type=bind,src=$(CURDIR),dst=/src,readonly" -w /work \
			$(WEB_TEST_NODE_IMAGE) sh /src/scripts/test-web-in-container.sh $(WEB_TESTS) ;; \
	*) echo "make test-web: WEB_TEST_RUNNER must be docker or local, not '$(WEB_TEST_RUNNER)'" >&2; exit 1 ;; \
	esac

test-ui: ## Check web/v1.css against v1, run Chromium smoke against a scratch server, then the Preact page tests (UI_JOBS=n runs n at once)
	node scripts/run-jobs.cjs -j $(UI_JOBS) --label ui --report tmp/ui-report-%p.json scripts/ui-tests.list

gate: ## The full gate: test-full and test-ui (GATE_UI_RUNS times, default 2) as two lanes side by side
	@sh scripts/gate.sh

dev: ## Run the server with live reload
	go tool air -c .air.toml
