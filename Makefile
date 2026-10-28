.PHONY: test test-race vet fmt fmt-check test-full test-ui test-web dev

# A bare `make` runs the Go tests, as it did before the progress runner.
.DEFAULT_GOAL := test

# Go tests run through scripts/gotestprogress, which turns `go test -json` into progress: each package's start and end with a count, every
# failing test with its output, and, when nothing happens for a while, which tests are running and for how long (see CONTRIBUTING.md,
# "Reading the test output"). It is built into tmp/ and rebuilt when its source changes. Its exit status is go test's.
# PLAIN=1 runs plain go test instead; PROGRESS_FLAGS passes flags to it (for example PROGRESS_FLAGS='-quiet 10s -hang 2m').
GOTESTPROGRESS := tmp/gotestprogress
PROGRESS_FLAGS ?=

$(GOTESTPROGRESS): $(wildcard scripts/gotestprogress/*.go) go.mod
	@mkdir -p tmp
	go build -o $@ ./scripts/gotestprogress

# gotest runs go test with the given arguments, through the progress runner unless PLAIN=1.
define gotest
	@if [ -n "$(PLAIN)" ]; then go test $(1); else $(GOTESTPROGRESS) $(PROGRESS_FLAGS) -- $(1); fi
endef

test: $(GOTESTPROGRESS) ## Run unit tests
	$(call gotest,./...)

test-race: $(GOTESTPROGRESS) ## Run tests with the race detector
	$(call gotest,-race ./...)

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go source
	gofmt -w cmd internal scripts

fmt-check: ## Fail if Go source is unformatted
	@test -z "$$(gofmt -l cmd internal scripts)" || { gofmt -l cmd internal scripts; exit 1; }

test-full: fmt-check vet test-race test-web ## Full local and CI gate

WEB_TESTS := sidebar-model claude-sessions claude-session-import system-events limit-guard sidebar-order preferences expiry room-unread room-notify inbox-notifications harness-offer notification-banner humanize time emoji highlight preview svg-headers macro-import settings-backup composer-markdown composer-paste markdown import-tree quick-commands message-usage role-defaults harness-model prompts-model macros-model roles-model general-model notifications-model usages-model quota-model users-model machines-model project-model shell-model health-model knowledge-model inbox-model agents-model members-model sender-profile-model preact-vendor spawn-model harness-version workspace-folder build-version import-problems

# The web tests are plain node scripts, with no npm dependencies. By default they run in Docker, from a digest-pinned image, so that every
# machine runs the same toolchain: the repository is mounted read-only at /src, copied to a tmpfs, and run from there with no network
# (scripts/test-web-in-container.sh). WEB_TEST_RUNNER=local runs them with the node on PATH instead; there is no automatic fallback either way.
# The pin must pass scripts/check-node-ca.cjs (no compromised CA roots (2026-10) in node's bundled or the system store): `make test-web` runs it first.
WEB_TEST_NODE_IMAGE ?= node:24.22.1-alpine3.24@sha256:06d7adf3e0ef61a13f1d28682726366f97a15aa1888c934d3ecec86c33918b53
WEB_TEST_RUNNER ?= docker

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

test-ui: ## Run Chromium smoke against a scratch server, then the Preact page tests
	node scripts/test-ui.cjs
	node scripts/test-harness-page.cjs
	node scripts/test-prompts-page.cjs
	node scripts/test-macros-page.cjs
	node scripts/test-roles-page.cjs
	node scripts/test-general-page.cjs
	node scripts/test-notifications-page.cjs
	node scripts/test-usages-page.cjs
	node scripts/test-quota-page.cjs
	node scripts/test-users-page.cjs
	node scripts/test-machines-page.cjs
	node scripts/test-project-page.cjs
	node scripts/test-claude-import-page.cjs
	node scripts/test-shell-page.cjs
	node scripts/test-health-page.cjs
	node scripts/test-knowledge-page.cjs
	node scripts/test-inbox-page.cjs
	node scripts/test-agents-page.cjs
	node scripts/test-spawn-page.cjs
	node scripts/test-room-members-page.cjs
	node scripts/test-sender-profile-page.cjs
	node scripts/test-sidebar-page.cjs
	node scripts/test-room-menu-page.cjs
	node scripts/test-mobile-tabs-page.cjs
	node scripts/test-chat-page.cjs

dev: ## Run the server with live reload
	go tool air -c .air.toml
