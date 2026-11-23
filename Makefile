# The Go release the checks run with; go.mod's go line stays the lowest version the module needs.
GO_TOOLCHAIN := 1.27.4
LINT := bin/golangci-lint
GO_TEST := go test -count=1 -race -shuffle=on
# The module's own runner, with its test product (internal/selftest).
WITH_DB := go run ./internal/selftest/withdb --
COUNT ?= 20
BASE ?= origin/main

export GOTOOLCHAIN := local
export GOFLAGS := -mod=readonly

.NOTPARALLEL:
.PHONY: qa fix unit stack keycloak flake mutate lint check-tools fmt-check vet tidy-check

qa: check-tools fmt-check vet lint tidy-check unit stack keycloak

fix: check-tools
	$(LINT) fmt
	$(LINT) run --fix
	go fix -tags database,stack,keycloak ./...
	go mod tidy

# Unit tests and the database tests of testdb, against the test product's own server.
unit:
	$(WITH_DB) $(GO_TEST) -tags database ./...

# The runner as a real process: it starts runs of its own.
stack:
	$(GO_TEST) -tags stack ./withdb

# The module's Keycloak, started once.
keycloak:
	go run ./internal/selftest/withdb --keycloak -- $(GO_TEST) -tags keycloak ./keycloak

flake:
	@if [ -z "$(TESTS)" ]; then echo "usage: make flake TESTS='<regexp>' [COUNT=20]"; exit 2; fi
	$(WITH_DB) $(GO_TEST) -count=$(COUNT) -tags database -run '$(TESTS)' ./...

# Mutation testing on the lines changed since BASE (.gomutants.yml).
mutate:
	@test -z "$$(git status --porcelain)" || { echo "make mutate needs a clean tree: commit first"; exit 1; }
	$(WITH_DB) go run github.com/gomutants/gomutants@v0.7.3 --workers 2 --changed-since $(BASE) --tags database \
		--cache=bin/gomutants-cache.json --output bin/mutation-report.json ./...

lint: check-tools
	$(LINT) run

check-tools:
	@./scripts/check-tools.sh $(GO_TOOLCHAIN)

fmt-check: check-tools
	$(LINT) fmt --diff

vet:
	go vet -tags database,stack,keycloak ./...

tidy-check:
	go mod tidy -diff
