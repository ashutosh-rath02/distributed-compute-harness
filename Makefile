# Works in Git Bash on Windows (needs `make`, e.g. from Git for Windows'
# extras or `choco install make`), Linux and macOS. Every target is a thin
# wrapper over a command or script you can also run by hand; see
# CONTRIBUTING.md.
SHELL := bash

.PHONY: build test integration lint fmt release-local clean

# Version, commit and date go into every binary (scripts/version.sh).
build:
	mkdir -p bin
	source scripts/version.sh && for c in manager agent relay harnessctl; do go build -trimpath -ldflags "$$VERSION_LDFLAGS" -o bin/$$c ./cmd/$$c || exit 1; done
	bash scripts/build-agents.sh

# Unit tests only. -p 1: packages run one at a time because several use
# fixed 127.0.0.1 ports.
test:
	go test -p 1 ./internal/... ./cmd/... ./scripts/...

# The black-box suite: fixed ports, so never alongside another test run.
integration:
	go test -p 1 ./test/integration

lint:
	go build ./...
	go vet ./...
	bash scripts/fmt-check.sh

fmt:
	gofmt -w $$(git ls-files '*.go')

# Everything a release ships, unsigned-by-CI but signed with your local key,
# into dist/release/.
release-local:
	bash scripts/package-release.sh

clean:
	rm -rf bin dist build
