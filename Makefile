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

WEB_TESTS := claude-sessions system-events limit-guard sidebar-order preferences expiry room-unread room-notify inbox-notifications harness-offer notification-banner humanize time emoji highlight preview svg-headers macro-import settings-backup composer-markdown markdown import-tree quick-commands message-usage role-defaults harness-model preact-vendor

test-web: ## Run the browser-free web unit tests (vm over the module source)
	@set -e; total=$(words $(WEB_TESTS)); n=0; \
	for name in $(WEB_TESTS); do \
		n=$$((n+1)); started=$$(date +%s); \
		printf '[web %d/%d] start  %s\n' $$n $$total $$name; \
		node scripts/test-$$name.cjs; \
		printf '[web %d/%d] ok     %s (%ds)\n' $$n $$total $$name $$(( $$(date +%s) - started )); \
	done

test-ui: ## Run Chromium smoke against a scratch server, then the Preact page tests
	node scripts/test-ui.cjs
	node scripts/test-harness-page.cjs

dev: ## Run the server with live reload
	go tool air -c .air.toml
