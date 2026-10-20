.PHONY: test test-race vet fmt fmt-check test-full test-ui test-web dev

test: ## Run unit tests
	go test ./...

test-race: ## Run tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go source
	gofmt -w cmd internal

fmt-check: ## Fail if Go source is unformatted
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }

test-full: fmt-check vet test-race test-web ## Full local and CI gate

test-web: ## Run the browser-free web unit tests (vm over the module source)
	node scripts/test-system-events.cjs
	node scripts/test-limit-guard.cjs
	node scripts/test-sidebar-order.cjs
	node scripts/test-preferences.cjs
	node scripts/test-expiry.cjs
	node scripts/test-room-unread.cjs
	node scripts/test-humanize.cjs
	node scripts/test-time.cjs
	node scripts/test-emoji.cjs
	node scripts/test-highlight.cjs
	node scripts/test-preview.cjs
	node scripts/test-svg-headers.cjs
	node scripts/test-macro-import.cjs
	node scripts/test-settings-backup.cjs
	node scripts/test-composer-markdown.cjs
	node scripts/test-markdown.cjs
	node scripts/test-import-tree.cjs

test-ui: ## Run Chromium smoke against a scratch server
	node scripts/test-ui.cjs

dev: ## Run the server with live reload
	go tool air -c .air.toml
