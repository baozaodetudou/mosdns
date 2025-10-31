SHELL := /bin/bash

# Use a clean environment without user-set GOROOT to avoid toolchain issues.
GO := env -u GOROOT go

.PHONY: build test tidy run

build:
	$(GO) build ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

run:
	$(GO) run .

