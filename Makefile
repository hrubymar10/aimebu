.PHONY: test test-race test-full vet cover fmt smoke-render help
.DEFAULT_GOAL := help

test:       ## Unit tests, fast (no race)
	go test ./...

test-race:  ## Tests under the race detector
	go test -race ./...

test-full:  ## Full pre-push gate: vet + race tests
	go vet ./...
	go test -race ./...

vet:        ## go vet
	go vet ./...

cover:      ## Tests with coverage summary
	go test -cover ./...

fmt:        ## Format all Go files
	gofmt -w .

smoke-render:  ## Headless render-lock: load the frontend in Chromium and fail on JS errors
	node scripts/smoke-render.js

help:       ## List targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-12s %s\n", $$1, $$2}'
